# kudurru architecture: the design record

> This is the v0.1 design as it was accepted, with the decision log (§16)
> that amends it. For how the code works today, with a diagram for every
> flow, read the [architecture tour](../docs/architecture/README.md); where the two
> differ, the tour follows the code and says so.

| | |
|---|---|
| Status | **Accepted baseline for v0.1** (2026-10-10); amend through the decision log in §16 |
| Revision | completeness/factual critic pass 2026-10-10: keepalived build source, leader-election lifecycle, PROXYv2 trust, internal-listener TLS, secrets-at-rest, session payload, licence of embedded list data, missing packages/interfaces; new decisions D44-D48, new open questions Q17-Q21 |
| Date | 2026-10-10 |
| Evidence | Research notes in [`dev-docs/research/`](research/), cited inline as `[gravity §18.1]`, `[k8s §13]` etc. Key: gravity, technitium, pihole, blocking (blocking-engines.md), dnsdist (dnsdist-keepalived.md), k8s (k8s-native-dns.md), auth (auth-oidc-rbac.md), integrations, go-dns-stack, observability (observability-querylog.md). Facts the notes mark UNVERIFIED are week-1 spikes, never assumptions |
| Provenance | Synthesised from three competing proposals (Kubernetes-first, single-binary-first, MVP-and-risk-first) scored by three judges (SRE, tech lead, product owner). The decision log in §16 names those proposals when it records a rejected alternative |
| License | Kudurru Community License for core DNS and dashboard (D166); folder-specific notices for `ee/` and `docs-site/`, source-viewing licenses for `website/` and `license-server/`. Business and government production requires an accepted commercial agreement and Order Form. Third-party licenses and Apache-2.0 catalog data remain separate |
| Module | `github.com/robertherbaugh/kudurru` — confirmed by the repo's `origin` remote (`https://github.com/robertherbaugh/kudurru`); ratified 2026-10-10 (D49) |
| Images / charts | `ghcr.io/robertherbaugh/kudurru`, `ghcr.io/robertherbaugh/kudurru-dnsdist`, `ghcr.io/robertherbaugh/kudurru-keepalived`, `oci://ghcr.io/robertherbaugh/charts/{kudurru,kudurru-crds}` (assumption, follows the module path) |
| CRD group | `kudurru.io` (confirmed by the project owner 2026-10-10, D49) |

**Thesis.** kudurru is one source-available Go binary that is a complete DNS server (authoritative with real serials, transfers and, later, DNSSEC; forwarding resolver, or per upstream group a validating recursive one (D170), with a node-local cache; blocky/AdGuard-grade blocking with client groups; Pi-hole-grade dashboard) and that is a first-class Kubernetes citizen **and** a `docker run` appliance without being two products. The object model is the product; persistence sits behind one `Store` interface with two drivers at v0.1 (Kubernetes CRDs via the apiserver; embedded SQLite) and **zero consensus code**, with `follower` (v0.2) and `raft` (v1.0) drivers arriving behind the same interface. dnsdist (edge/load balancing) and keepalived (VIP) are GPL siblings driven by a kudurru supervisor process inside their own images, never linked. Every deferral names the seam that keeps it additive.

The boundary-stone metaphor is the data model: a `DNSZone` is a recorded grant of authority (the stone kept in the temple = the store that is authoritative for configuration); clay copies are distributed by standard DNS (AXFR/IXFR, NOTIFY, TSIG, catalog zones) and by replication of the store itself.

---

---

## 1. Vision & principles

kudurru should feel to a platform engineer the way cert-manager or external-dns feel (install a chart, apply a few CRs, watch `kubectl get zones` turn `Ready`) and to a home-lab user the way Pi-hole feels (one container, one dashboard that explains every answer), with Technitium's zone depth and blocky/AdGuard's blocking underneath. Principles — each one is a rule used in a later section:

1. **Correct before complete.** v0.1 ships fewer zone types and no DNSSEC signing rather than wrong serials, wrong CNAME handling, or NXDOMAIN from an unsynced source [gravity §18.1, k8s §2.2].
2. **One object model, one `Store` interface, zero consensus code in v0.1.** In Kubernetes the apiserver is the replicated store (CRDs are the truth, Deployment, no PVC for config) [k8s §13 model 1, §16.2]; in Docker it is embedded SQLite. `follower` (v0.2) and `raft` (v1.0) drivers arrive behind the same interface [go-dns-stack §6.3].
3. **Exactly one writer per object.** `metadata.managedBy` (`api | file:<path> | integration:<name> | bootstrap | system`; in Kubernetes the apiserver's field manager) makes "two sources of truth" structurally impossible [k8s §16.1, gravity §18.2].
4. **Serve from memory; degrade, never stop.** The query path reads lock-free snapshots, never the store; replicas keep answering from the last good snapshot (persisted to local disk) when the store is unreachable; before the first sync they answer SERVFAIL, never NXDOMAIN [k8s §8, gravity §4.4].
5. **Replicate configuration; never caches, logs or heartbeats.** Query logs are per-node SQLite with scatter-gather views; metrics go to Prometheus; node status is scatter-gathered, not written anywhere [gravity §18.1/§18.8, observability §5]. Opt-in exception: with `database.dsn` every node also ships its audit events, query log and statistics to a central PostgreSQL database (D158), always writing its local SQLite first.
6. **Fixed pipeline, named stages.** Stage order is code, not config; integrations attach to named stages and never run on the query path [technitium §3, integrations §1.2].
7. **Records are external-dns `Endpoint`s (a superset).** `DNSEndpoint` ingestion, the external-dns webhook, the Cloudflare sink and the CRD spec share one shape [k8s §3.2, integrations §1.6].
8. **Leader-only side effects.** Status writes, serial stamping, blocklist fetch, NOTIFY, DNSSEC signing, ACME and integrations run on exactly one node (Lease / self / raft leader); serving runs on every node [k8s §8].
9. **GPL stays outside the binary.** dnsdist and keepalived are separate processes driven by rendered files, the dnsdist HTTP API, the console binary, signals, `track_file` and the notify FIFO; Technitium, AdGuard Home, Gravity and Pi-hole are idea sources only; blocky, dnsproxy and CoreDNS are the only upstreams whose code is copied or imported [blocking §1.1, dnsdist §4].
10. **Secure by default, lessons from the 2026 CVEs of the comparables:** no code-bearing or privileged setting reachable through the API (`hostOnly` keys), hashed tokens with constant-time compare, Argon2id with server-fixed parameters, sealed HttpOnly cookies, bounded metric labels, probes on an unauthenticated ops port, Restricted PSS out of the box [pihole §17.7, technitium §21.10, auth §10].
11. **Multi-node from day one in the UI.** Node selector + "all nodes" on every stat, "which node answered" on every log row, even with one node [pihole §17.2].
12. **Every deferral has a seam.** §13.1 lists, per deferred feature, the interface or schema field that already exists in v0.1.

---

## 2. Language, stack, key libraries

Go, because every reusable component (CoreDNS, blocky, dnsproxy, external-dns, controller-runtime) is Go and shares `miekg/dns` v1 types; Rust's hickory-dns has an experimental recursor and no IXFR [go-dns-stack §1]. Versions are the ones verified in the research on 2026-10-10.

| Concern | Choice | Version | Licence | Notes |
|---|---|---|---|---|
| Toolchain | Go | **1.27** (`go 1.27` in go.mod, toolchain pinned to latest 1.27.x; CI on 1.27 + tip) | BSD-3 | 1.26/1.27 are the supported majors; miekg v2 needs 1.27 anyway [go-dns-stack §13] |
| DNS wire | `github.com/miekg/dns` | v1.1.73 | BSD-3 | fixes-only line; confined to `dnsserver`, `zone/xfr`, `upstream`, `recursor`, `dnssec` behind `zone.RRset` so a `codeberg.org/miekg/dns` v2 migration is contained [go-dns-stack §2.4] |
| Upstream clients | `github.com/AdguardTeam/dnsproxy/upstream` | v0.86.0 | Apache-2.0 | UDP/TCP/DoT/DoH(1.1/2/3)/DoQ/DNSCrypt/sdns + bootstrap; strategies are ours [blocking §6.5] |
| QUIC/HTTP3 | `github.com/quic-go/quic-go` | v0.63.0 | MIT | DoQ/DoH3 servers patterned on CoreDNS `server_quic.go`/`server_https3.go` (v0.1, D67) |
| Matchers, list pipeline, cache ideas | copied from `github.com/0xERR0R/blocky` | v0.35.0 | Apache-2.0 | `cache/stringcache`, `lists/`, caching ideas; NOTICE attribution [blocking §7] |
| Adblock-DNS grammar | clean-room parser from the AdGuard KB | n/a | — | `urlfilter`/AdGuard Home are GPL-3.0 and are never imported [blocking §1.1] |
| Embedded store, local snapshot, query log, audit | `modernc.org/sqlite` | v1.54.0 (SQLite 3.53.3, cgo-free) | BSD-3 | **one engine** for `state.db`, `local.db`, `querylog.db`; distroless-static friendly [go-dns-stack §6.2] |
| Replication (v1.0) | `github.com/hashicorp/raft` + `raft-boltdb/v2` | v1.8.0 / v2.3.1 | MPL-2.0 | unmodified dependencies; SQLite FSM; single-voter bootstrap (rqlite model); deferred to v1.0 (§4.4) |
| Kubernetes | `k8s.io/client-go`, `sigs.k8s.io/controller-runtime`, `k8s.io/apiextensions-apiserver` (schema + CEL validator packages), controller-gen | v0.37.1 / v0.25.x | Apache-2.0 | Lease election, `NeedLeaderElection()` runnable split [k8s §7.4]; the apiextensions validator runs locally in Docker mode [k8s §13] |
| CEL | `github.com/google/cel-go`, pinned to the version `k8s.io/apiextensions-apiserver` v0.37.1 imports so the binary carries one CEL copy; bumped only with the Kubernetes minor (D74). The `cel-expr/cel-go` path move noted in the auth research is not used by apiextensions yet | v0.29.2 | Apache-2.0 | CRD `x-kubernetes-validations` in Docker mode; OIDC claim expressions (v0.2) |
| Config | `github.com/knadh/koanf/v2` | v2.3.x | MIT | file + env (`KUDURRU_<SECTION>__<KEY>`) + flags; no key lowercasing [go-dns-stack §8] |
| CLI | `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 | kubectl-style `get/apply/delete -f` |
| HTTP API / OpenAPI | `github.com/danielgtaylor/huma/v2` on Go 1.22+ `net/http` mux | v2.38.0 | MIT | code-first OpenAPI 3.1; TS client via `openapi-typescript`; Go client reused by the CLI |
| OIDC | `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` | v3.21.0 / v0.37.0 | Apache-2.0 / BSD-3 | discovery, JWKS, PKCE, device flow [auth §3] |
| Passwords | `golang.org/x/crypto/argon2` | current | BSD-3 | Argon2id m=19 MiB, t=2, p=1, PHC strings [auth §7] |
| Sessions | sealed cookies (AES-256-GCM) behind a `SessionStore` interface; `db` implementation on SQLite (v0.2) | — | — | §8.2 |
| Metrics / tracing / logs | `prometheus/client_golang`, OTel Go SDK, `log/slog` | v1.24.1 / v1.45.0 / stdlib | Apache-2.0 | bounded-label policy [observability §3.2]; no logrus/zap |
| dnstap (v0.2) | `github.com/dnstap/golang-dnstap` | v0.4.0 | Apache-2.0 | `SocketWriter`, listen mode |
| TLS/ACME (v1.0) | `github.com/caddyserver/certmagic` | v0.25.4 | Apache-2.0 | DNS-01 against kudurru itself, leader-only |
| Cloudflare (v0.2) | `github.com/cloudflare/cloudflare-go/v7` | **exact pin** 7.12.0 | Apache-2.0 | thin wrapper; minors break [integrations §2.2] |
| UniFi (v0.2) | `github.com/unpoller/unifi/v6` | 6.3.1 | MIT | official Integration API + legacy read paths [integrations §3.4] |
| Files | `github.com/fsnotify/fsnotify` | v1.9.0 | BSD-3 | config, object dir, cert reload |
| Release | goreleaser `kos:` + ko + cosign keyless | 2.18 / 0.19.1 / 3.1.3 | — | `gcr.io/distroless/static:nonroot` pinned by digest; `CGO_ENABLED=0` is a release requirement [go-dns-stack §12] |
| Frontend | Bun + React 19.2 + TypeScript + **Astryx** (`@astryxdesign/core`, `@astryxdesign/theme-*`; Meta's design system, Beta, pinned exactly) + TanStack Query (+ Table/Virtual for the query log) + uPlot 1.6.32 + ECharts 6.1 | — | MIT | pure SPA via `go:embed`; information architecture from Pi-hole; stack per D69 (Astryx charts adopted once stable) |
| Edge (separate containers) | `powerdns/dnsdist-21` (2.1.3, Debian packages upgraded at build), keepalived **2.4.3 compiled from the upstream release tarball** in an Alpine builder stage (Alpine's `apk` package is still 2.3.4 [dnsdist §2.1] and lacks `auth_hmac`, which the rendered config needs) | pinned by digest | GPL-2.0 / GPL-2.0+ | driven by `kudurru edge run` / `kudurru vip run` supervisors (§3.6); building from source means the image README must carry the GPL text **and** a written source offer (tarball + build script kept in `images/keepalived/`) |

Explicitly **not** used: embedded etcd (1 GB floor, bespoke join, fsync-sensitive [go-dns-stack §6.2, gravity §16]), dragonboat, marmot, Litestream-as-replication, DuckDB/mattn (cgo), `urlfilter`/AdGuard Home packages (GPL), go-plugin as the primary extension API, Casbin/OpenFGA as the primary authorizer, viper, logrus/zap, a bespoke dnsdist console client (we exec the `dnsdist` binary).

---

## 3. Component architecture

### 3.1 One binary, roles, runnables

`kudurru` runs a set of **roles** selected by config (`roles: [dns, api, controller, querylog, ops]`, Gravity-style [gravity §17.1]) scheduled by a controller-runtime-shaped manager where each runnable declares whether it is **always-on** (every replica) or **leader-only** [k8s §8]. Every role implements `Ready() (bool, []string)` and feeds `/healthz/ready`.

| Runnable | Leader-only? | What it does |
|---|---|---|
| `dnsserver` (UDP/TCP/DoT/DoH/DoQ/DoH3; PROXYv2) | no | serves queries through the pipeline |
| `zonecache` (store watch → in-memory trees/indexes; local snapshot persistence) | no | materialises zones/records/policies/lists from the store; writes `local.db` snapshot; readiness = all watches synced once **or** serving from a fresh-enough snapshot (§4.4) |
| `api` (HTTP API + embedded UI + SSE) | no | any replica serves any user (sealed sessions, §8.2) |
| `querylog` (rings → SQLite → rollups → sinks) | no | per node |
| `ops` (`:8081/healthz/*`) + `metrics` (`:9153/metrics`) | no | unauthenticated ops listeners, never on the DNS or API port |
| `internal` (`:8082`, node-to-node: list snapshots, status, events) | no | **always TLS**: node certificate and the bearer token are HKDF-derived from the cluster encryption key (`KUDURRU_ENCRYPTION_KEY` / `kudurru.io/session-key` Secret) unless `tokenFile`/`tls` are set explicitly, so every node sharing the key trusts every other and the chart needs no extra Secret; used by replicas, edge supervisors and scatter-gather; v0.2 `/internal/snapshot` carries internal kinds and therefore never runs without TLS |
| `controller` (one reconciler per kind; status, serial stamping, duplicate detection, Events) | **yes** | writes `status.conditions`, zone serials |
| `listfetcher` (blocklist download + digest publication) | **yes** | conditional GET, disk cache, digest in status; replicas pull by digest (§7.3) |
| `notifier` (NOTIFY fan-out; XFR scheduling for secondaries v0.2) | **yes** | coalesced NOTIFY per zone |
| `signer` (DNSSEC, v1.0) | **yes** | signs on the leader; signed RRsets replicate by IXFR (§4.6) |
| `integrations` (Cloudflare, UniFi, webhook; v0.2) | **yes, per instance** (`Lease integration-<ns>-<name>`) | level-triggered reconcile loops off the query path |
| `acme` (v1.0) | **yes** | DNS-01 against kudurru's own zones |
| `edge run` / `vip run` | n/a | supervisors inside the dnsdist / keepalived images (§3.6) |

`store.Elector` is backed by a Kubernetes `Lease` (kube driver), "always leader" (sqlite driver), "never leader" (follower driver, v0.2) and raft leadership (v1.0).

### 3.2 Diagram

The planned shape. The v0.1.0 node (seven stages, no DNSSEC signer or
Cloudflare/UniFi providers) is drawn in the [tour's overview](../docs/architecture/README.md).

```
                      clients (Do53 / DoT / DoH / DoQ / DoH3)
                                   │
            ┌──────────────────────▼───────────────────────┐   optional edge tier (GPL, separate container)
            │ dnsdist 2.1  :53/:853/:443  packet cache      │   PID 1 = `kudurru edge run` (render → check → exec)
            │ pools: resolver(chashed) authoritative(leastOutstanding)  DynBlocks (eBPF opt-in) │
            └──────────────────────┬───────────────────────┘
                     PROXYv2 (client IP preserved)  → :5353 (or :53 when no edge)
┌───────────────────────────────────▼─────────────────────────────────────────────────┐
│ kudurru node (every replica)                                                         │
│  dnsserver ─▶ PIPELINE  ingress ▸ identify ▸ control ▸ local ▸ authoritative ▸ blocking │
│                      ▸ rewrite ▸ cache ▸ dnssec-validate ▸ upstream ▸ postprocess ▸ log │
│       ▲ reads lock-free snapshots (atomic.Pointer) of:                                   │
│       │   zone trees │ policy/clientgroup index │ compiled blocklists │ upstream groups   │
│  zonecache ◀── Store.Watch(kind…) ──────────────┐  ──▶ local.db (last-good snapshot)      │
│  api (huma, UI, SSE)  querylog (ring→SQLite→rollups→sinks)  ops/metrics  internal :8082 │
│  controller / listfetcher / notifier / signer / integrations / acme  [leader-only] ─┼─┐ │
└──────────────────────────────────────────────────────────────────────────────────────┼─┼─┘
   STORE (config plane)   kube driver: CRDs via apiserver (replicated by k8s etcd)  ◀──┘ │
                          sqlite driver: /var/lib/kudurru/state.db (docker)        ◀────┘
                          follower (v0.2): sqlite mirror pulled from a primary; raft (v1.0): sqlite FSM
   RUNTIME (per node)     querylog.db, local.db (snapshot, audit, cache dump), lists/, keys/
   VIP (bare metal)       keepalived (VRRPv3) with PID 1 = `kudurru vip run`; in k8s: kube-vip / MetalLB / cloud LB
```

### 3.3 Dataplane packages

- `internal/dhcp`, `internal/dhcpdns`, `internal/dhcpsetup` and `internal/dhcpha` (v0.1.0, D172–D180): DHCPv4 standalone or three-node quorum failover on Linux/SQLite, or `DHCPScope`/`DHCPLease` objects served by one elected replica on the Kubernetes store (D180). Direct and trusted-relay scopes, durable leases, conflict probes, advanced network/boot options, reservations, live API management and atomic import/export. Committed memory snapshots provide persistent A/PTR name ownership and policy hostnames. Dashboard setup stages topology for restart; established quorum scopes use replicated live options and fixed shared topology. See [DHCP architecture](../docs/architecture/dhcp.md).
- `internal/dnsserver`: listeners on `dns.Server` (UDP/TCP with `ReusePort`, N sockets = GOMAXPROCS like CoreDNS `multisocket`), `tcp-tls` for DoT, `net/http` for DoH h1.1/h2, PROXYv2 parser restricted to `listeners.proxyProtocolFrom` (hard requirement because dnsdist fronts us [dnsdist §1.13]) **and** to headers carrying TLV `0xE0` equal to the shared `listeners.proxyProtocolSecretFile` value (32 random bytes; dnsdist adds it with `SetAdditionalProxyProtocolValueAction`, the supervisor renders it). Source-CIDR trust alone is spoofable by any host in the trusted range (any pod in the pod CIDR, any LAN host on a /24), which would let an attacker forge the client address that `queryACL`/`transferACL`/client-group policies and the query log rely on; headers without a matching TLV are dropped and counted in `kudurru_dns_dropped_total{reason="proxy_unauthenticated"}`. Loopback (`127.0.0.1/32`, `::1/128`) is exempt when the secret is unset but must still be listed. Semantics pinned by the spike (D72, D76): headers are optional and a header-less query is attributed to its transport peer; `LOCAL` and `AF_UNSPEC` headers need the same authentication and keep the peer address; the TLV value is the secret file's bytes with trailing CR/LF trimmed, at least 32 bytes; CIDR entries in `proxyProtocolFrom` are accepted **only** when `proxyProtocolSecretFile` is set (pod IPs are dynamic; the TLV authenticates, the CIDR only narrows exposure), exact `/32`s otherwise. Transport limits: `MaxTCPQueries` unlimited with a 10 s idle timeout (miekg's default closed TCP/DoT after 128 queries), UDP read buffer 4096 B plus 2 KiB when PROXYv2 is on (the 512 B default silently dropped large EDNS queries), DoH body cap 65,535 B and headers 16 KiB, DoQ grants no unidirectional streams, QUIC connection cap configurable. All four TLS listeners share one certificate reloader per cert/key pair (fsnotify plus a 30 s poll); a half-rotated pair keeps serving the previous certificate. DoQ/DoH3 (v0.1, D67) copy CoreDNS/dnsproxy server code (ALPN `doq`/`h3`, 256 streams, 1024 workers, RFC 9250 error codes; never reuse a fixed DNS ID when bridging DoQ→UDP [go-dns-stack §4.1, §17.9]).
- `internal/pipeline`: the fixed stage chain (§7.1). Stages are packages under `internal/stages/`: `ingress`, `identify`, `control`, `local`, `authoritative`, `blocking`, `rewrite`, `cache`, `dnssec`, `upstream`, `postprocess`, `log`.

```go
// internal/pipeline/stage.go
type Stage interface {
    Name() string                                   // fixed, documented stage id
    Serve(ctx *Request, next Next) (*Response, error)
}
type Next func(*Request) (*Response, error)
type Request struct {
    Ctx           context.Context
    Msg           *dns.Msg       // miekg only here and in dnsserver/xfr/upstream/dnssec
    Client        Client         // addr, proto, proxied-from, clientID (SNI/DoH path), MACs, GroupIDs (identify),
                                 // TSIGKey (set only after the transport verified the signature)
    Received, Deadline time.Time
    Zone          any            // *zone.Zone, set by authoritative when a zone matched
    UpstreamGroup string         // forward zone or Policy group for the upstream stage; beats the conditional map, never cached
    Policy        any            // *policy.Decision from identify (read with policy.FromRequest; shared, immutable)
    // Trace TraceInfo           (v0.2, with OTel)
}
type Response struct {
    Msg        *dns.Msg
    Status     Status              // taxonomy in §11.2 (forwarded, cached, authoritative, blocked_list, …)
    Upstream   string              // dictionary name, never a raw IP
    List, Rule string              // matched list and rule text (the UI "why" row)
    Cacheable  bool                // local/conditional answers are never cached (blocky rule)
    DNSSEC     DNSSEC              // secure/insecure/bogus from recursive groups (D170), kept by the cache; unknown otherwise
    Transfer   iter.Seq[*dns.Msg]  // AXFR/IXFR stream; TCP/DoT write every message, Msg is the first
}
```
Optional capability interfaces discovered by type assertion (CoreDNS style): `Ready() bool`, `Transfer(zone string, serial uint32) (<-chan []dns.RR, error)`, `Reload(snapshot any)`.

- `internal/zone`: the record model (§4.1), per-zone in-memory tree (CoreDNS `plugin/file/tree` red-black tree, Apache-2.0), wildcard lookup, CNAME chasing (loop cap 20), SOA/serial management, journal for IXFR, AXFR/IXFR out via `dns.Transfer`, coalesced NOTIFY out; secondary-in (v0.2) copies the CoreDNS `file/secondary.go` flow [go-dns-stack §5].
- `internal/blocking`: compiled matcher chain (§7.3), clean-room adblock parser, snapshot compile.
- `internal/cache`: sharded TTL message cache with min/max/negative TTL, prefetch (threshold/window), serve-stale (RFC 8767: stale TTL 3 d, stale answer TTL 30 s, max wait 1.8 s — Technitium numbers), in-flight coalescing, OPT stripped, DO-aware keys; persisted to `local.db` on shutdown [technitium §20.8, blocking §6.5].
- `internal/upstream`: dnsproxy upstream groups with strategies `parallel_best` (default), `strict`, `random`, `rtt_weighted`; conditional forwarding by longest suffix; bootstrap list; dnsmasq `[/domain/]` import syntax. A group with `mode: recursive` has one server, `<group>/recursive`, backed by `internal/recursor` (D170).
- `internal/recursor` (v0.1, D170): kudurru's own iterative resolver behind dnsproxy's upstream interface, shared by every recursive group and kept across reloads: built-in root hints with priming (RFC 9609), delegation cache, bailiwick-checked glue, QNAME minimization (RFC 9156), 0x20 and strict question matching, RFC 6303 and special-use names answered locally, per-query work limits (NXNS, KeyTrap, RFC 9276), and DNSSEC validation from built-in root trust anchors (RFC 4033-4035, 5155, 6840); bogus answers become SERVFAIL with EDE 6-12 unless the client set CD.

### 3.4 Control-plane packages

- `api/v1alpha1`: Go types for every kind with kubebuilder markers. **The same structs are what the sqlite driver persists (JSON)** and what the API/CLI/UI exchange. CRD OpenAPI + CEL validation is executed locally in Docker mode with the apiextensions validator so both modes reject the same inputs [k8s §13].
- `internal/store`: the `Store` interface (§4.3), drivers `kube`, `sqlite`, `follower` (v0.2), `raft` (v1.0), the shared validator, the schema version/migration contract, and the **conformance suite** every driver must pass.
- `internal/controller`: one reconciler per kind (never a mega-controller), idempotent, level-triggered, writing `metav1.Condition`s + `observedGeneration`; finalizers for external cleanup; `Events` on every operational transition (`SerialBumped`, `NotifyFailed`, `ListFetchFailed`, `PublishConflict`, `Duplicated`).
- `internal/config`: koanf loader, JSON Schema, and the **settings metadata table** (`schema.go`: key, type, default, help, `restartRequired`, `envLocked`, `writeOnly`, `hostOnly`) that drives file, env, `kudurru config`, `PATCH /node/config`, the "All settings" page and the docs page; a test asserts docs cannot drift.
- `internal/auth`, `internal/rbac`: §8. `internal/rbac/permissions.yaml` is the single source for roles, aggregated ClusterRoles, docs and the route-permission lint.
- `internal/api`: huma routes under `/api/v1`, uniform envelope (`took_ms`, `{error:{key,message,hint}}`), 201+Location, 204 on delete, `:batchDelete`, keyset pagination, SSE; Scalar docs at `/api/docs` at the running version.
- `ee/integration`: registry, manifest, lifecycle, leader-scoped runner, plan engine, ledger (§9).
- `ee/edge/{dnsdist,keepalived}`: renderers + supervisors (§3.6).
- `internal/cluster`: node identity, membership source abstraction (static | endpointslice | follower | raft), scatter-gather client, internal listener, promote/fencing (v0.2).
- `internal/querylog`, `internal/audit`, `internal/metrics`, `internal/health`: §11.

### 3.5 UI and CLI

- UI: SPA at `/`, talks only to `/api/v1`; node selector and "all nodes" scope first-class from v0.1 even with one node. Pages in §10.
- CLI `kudurru` (seven letters): `serve`, `login`, `get|apply|delete|describe -f`, `zones`, `records`, `lists`, `blocking enable|disable 5m`, `query` (dig-like), `health [--vrrp]`, `lameduck`, `backup|restore|export`, `import bind|pihole|adguard|blocky|technitium`, `edge render|run`, `vip render|run`, `cluster status|promote|join-token` (v0.2+), `integrations list|check|sync|preview|webhook conformance` (v0.2), `admin reset-password|create-token` (unix socket, uid-gated), `config get|set|schema`, `doctor`, `api <path>`, `version`, `completion`. All commands use the generated OpenAPI client except `serve`, `admin`, `doctor` and the supervisors. `--context` picks a kubeconfig (CRD verbs go straight to the apiserver) or a kudurru API endpoint.

### 3.6 dnsdist and keepalived: supervisors, not sidecars, not shell loops

Both tools are GPL-2.0 and must remain separate programs [dnsdist §4]. The dnsdist REST API cannot manage backends, YAML has no reload, and membership changes go through the console (whose behaviour when dnsdist was started from YAML is UNVERIFIED) [dnsdist §1.11, §1.12, §1.16, §7]. The lowest-risk orchestration is a **kudurru supervisor process that is PID 1 in the same image as the tool** (mere aggregation: two separate executables, GPL text and source offer shipped in the image README).

**`ghcr.io/robertherbaugh/kudurru-dnsdist:<dnsdist>-k<kudurru>`** = `FROM powerdns/dnsdist-21@sha256:<digest>` + `/usr/local/bin/kudurru`. Entrypoint `kudurru edge run`:

1. Build a `Topology{Binds, TLS, Pools, Backends[], Cache, Console, Webserver, Rules, DynBlocks}` from flags/config and the membership source: `--backends 10.0.0.11:5353,…` (static, compose), `--backends-from endpointslice:<ns>/<headless-svc>` (Kubernetes), `--backends-from cluster` (follower/raft, v0.2+). Backend `id` = `uuid5(kudurru-node-name)` so the `chashed` ring is stable; `use_proxy_protocol: true`; health check `health.kudurru.internal.` TXT with `must_resolve: true` and a 2.1 `validation_function` checking the node id (`interval 1, max_failures 2, rise 2`).
2. Render `dnsdist.yml` (every `backends[]`, `binds[]`, `pools[]` `resolver` (`chashed`, packet cache) and `authoritative` (`leastOutstanding`, **uncached by default**; `edge.authoritativeCache` enables a short `max_ttl` with flush-on-serial-change), `query_rules` refusing XFR/NOTIFY/UPDATE from unknown sources, `dynamic_rules`, `webserver`/`console` loopback-only with credentials generated per start; the API key is also written to `/run/kudurru/dnsdist-api.key` (0640, group `keepalived_script`) on the shared `kudurru-run` volume so `kudurru health --vrrp` in the keepalived container and the local kudurru's `/edge` page can read it) plus `dnsdist.lua` with **functions only** (never `enableLuaConfiguration()`) [dnsdist §6.5].
3. `dnsdist --check-config -C candidate.yml`; atomic rename; exec `dnsdist --supervised --disable-syslog -C /etc/dnsdist/dnsdist.yml` as a child; forward SIGTERM; expose `:8084/healthz/ready` = `GET /api/v1/servers/localhost` (X-API-Key) reports ≥ 1 backend `up` — so Kubernetes probes the supervisor, not the API key.
4. On membership change — **v0.1 baseline:** regenerate YAML and restart the child (readiness 503 → drain → sub-second restart; invisible behind a VIP/LB with ≥ 2 edge replicas; the packet cache is lost on every restart) [dnsdist §1.16 option 3]. Membership events are debounced 5 s and only endpoints with `conditions.ready` (or `serving` during a rollout) are rendered, so a kudurru Deployment rollout restarts each edge replica once per surge batch rather than once per pod; a lone edge replica (compose `edge` profile, single-node k8s) accepts the ~0.5 s blip, which keepalived's `fall 3` does not count as a VIP failure. **v0.2, gated on the week-1 spike:** `dnsdist -C dnsdist.yml -e 'newServer({...})'` / `getServer("<uuid>"):setDown()` → 5 s purge delay → `rmServer("<uuid>")`, then regenerate YAML so a restart converges [dnsdist §1.16 option 1]. If the spike fails, the supervisor renders Lua (`dnsdist.conf`) instead — both renderers share `Topology`.
5. Cert change (fsnotify) → v0.1 restart; v0.2 `reloadAllCertificates()` over the console. Zone serial change (polled from `GET http://<backend>:8082/internal/zones` every 5 s; SSE in v0.2) → `DELETE /api/v1/cache?pool=authoritative&suffix=<zone>` when the authoritative pool is cached. UI "block this client at the edge for 10 min" → Lua `registerWebHandler('/kudurru/dynblock')` → `addDynamicBlock` (v0.2).

**`ghcr.io/robertherbaugh/kudurru-keepalived:<keepalived>-k<kudurru>`** = Alpine multi-stage build compiling keepalived 2.4.3 from the release tarball (upstream `Dockerfile.in` pattern: `--enable-json`, VRRP only, no LVS/BFD, user `keepalived_script`) + `/usr/local/bin/kudurru`. Alpine's package is 2.3.4 [dnsdist §2.1], which has no `auth_hmac`; if an operator forces a 2.3.x binary (`vip.keepalived.legacy: true`) the renderer drops `auth_hmac`, keeps `check_unicast_src` + `unicast_fault_no_peer`, and `doctor` warns that adverts are unauthenticated. Entrypoint `kudurru vip run`, full contract:

- Render `keepalived.conf`: `global_defs { router_id <node>; enable_script_security; script_user keepalived_script; vrrp_version 3; vrrp_garp_master_refresh 30; vrrp_notify_fifo /run/kudurru/vrrp.fifo; vrrp_notify_fifo_script "/usr/local/bin/kudurru vip fifo" }`; per VIP a `vrrp_instance` with `state BACKUP`, `nopreempt`, deterministic `priority 200 - 10*idx` (never 255), `advert_int 1`, `unicast_src_ip`, `unicast_peer {…}`, `unicast_fault_no_peer`, `check_unicast_src`, `auth_hmac { key 1 file:/run/secrets/vrrp_hmac_<vrid>; active_key 1; anti_replay monotonic }`, `virtual_ipaddress { <addr>/<len> dev <iface> }`, `track_script { chk_<vip> }`, `track_file { tf_<vip> }`.
- `vrrp_script chk_<vip>` = `kudurru health --vrrp --dns <vip>:53 --qname health.kudurru.internal. --dnsdist-api http://127.0.0.1:8083 --dnsdist-api-key-file /run/kudurru/dnsdist-api.key --require-backends 1` (probes the VIP address on :53 **udp and tcp** while MASTER and loopback while BACKUP — a loopback-only check would pass with a misbound dnsdist [dnsdist §2.7] — and requires ≥ 1 healthy backend via the dnsdist API; `interval 1 timeout 1 fall 3 rise 2 weight 0` → FAULT on failure; runs as `keepalived_script` under `enable_script_security`, so the binary path is root-owned and the key file is group-readable).
- `track_file tf_<vip>` = `/run/kudurru/weight-<vip>`, `weight -1`: the local kudurru writes `0` (healthy serving member), `60` (prefer-not: lowers the effective priority so this node loses the *next* election), `300` (→ FAULT: must not hold the VIP) [dnsdist §2.6]. **Because every instance runs `nopreempt`, a priority reduction alone never moves a VIP away from the current MASTER** (a higher-priority BACKUP does not preempt by design); only FAULT does. **Decision D51 (project owner, 2026-10-10): `kudurru lameduck` never touches the VIP.** Lameduck makes the local kudurru fail dnsdist's backend health check, so the node drains out of every edge's backend pool while the VIP stays with it for as long as its own dnsdist is healthy with ≥ 1 backend. The VIP moves only on real failure (dnsdist down, no healthy backend, node loss, FAULT from `track_script`) or when an operator explicitly runs `kudurru vip drain` / maintenance mode, which writes `300`. `60` is reserved for "not the leader / not a voter" hints that only matter when the VIP is next re-elected. The 2-VM e2e asserts three behaviours: lameduck keeps the VIP, `vip drain` moves it within `fall`/down-timer time, node loss moves it (spike (h), §14).
- `keepalived -t -f candidate.conf` before swap; atomic rename, previous file kept; `SIGHUP` the child on re-render; `stop_grace_period: 20s`; the VRID is a `hostOnly` config value and never auto-allocated or changed on re-render.
- FIFO consumer emits `kudurru_vip_master{vip}`, audit `cluster.vip.changed`, event `vip.master_changed`; never blocks.
- Optional second VIP with reversed priorities = active/active across two resolver addresses. With `nopreempt`, after a failover and recovery both VIPs stay on the surviving node until `kudurru vip rebalance` (writes `300` for the VIP that should move, then `0`) or the operator sets `vip.preemptDelay: 60` on the second VIP instead of `nopreempt`.
- **Every node's dnsdist lists all kudurru nodes as backends**, so VIP ownership follows dnsdist health, not kudurru health: a node whose kudurru is unhealthy but whose dnsdist is fine still serves [dnsdist §3.2].

Both supervisors ship in **v0.1** (compose `edge` and `ha-static` profiles; Kubernetes edge Deployment behind `edge.enabled`). keepalived is never the default VIP in Kubernetes [k8s §16.14, dnsdist §6.12]. Alternative for operators who must run unmodified upstream images: `edge.image.mode: upstream` runs the agent as a sidecar sharing an `emptyDir` and converges by restart only (documented, non-default).

The VIP abstraction the risk table (§14 #4) refers to:

```go
// ee/edge/vip/provider.go — implemented by keepalived (track_file + FIFO), kube-vip and MetalLB (Service status + annotations), none
type Provider interface {
    Name() string                                   // keepalived | kube-vip | metallb | none
    VIPs() []VIP                                    // {Name, Address, VRID?, Owner string, Weight int}
    Owner(vip string) (node string, ok bool)        // current MASTER / announcing node, from FIFO or Service status
    SetWeight(ctx context.Context, vip string, w Weight) error   // Weight = Serving(0) | PreferNot(60) | Fault(300); no-op for Service-based providers
    Events() <-chan VIPEvent                        // {VIP, Node, State MASTER|BACKUP|FAULT|STOP, Priority, At}
}
```

---

## 4. Data model, storage, replication, HA

### 4.1 Objects

All objects are `api/v1alpha1` Go structs with `metadata{name,namespace?,labels,annotations,uid,generation,resourceVersion,creationTimestamp,managedBy}`, `spec`, `status{observedGeneration,conditions[]}`. Names are stable keys. Record data is the external-dns `Endpoint` shape plus a `kudurru` extension block.

```go
// api/v1alpha1/dnszone_types.go (cluster-scoped)
type DNSZoneSpec struct {
    ZoneName     string         `json:"zoneName"`                 // immutable (CEL self==oldSelf), FQDN with trailing dot
    Type         ZoneType       `json:"type"`                     // primary | forward | secondary(v0.2) | stub(v0.2) | external(v0.2) | catalog(v1.0) | secondaryCatalog(v1.0)
    SOA          *SOASpec       `json:"soa,omitempty"`            // mname, rname, refresh, retry, expire, minimum; serial lives in status
    Nameservers  []string       `json:"nameservers,omitempty"`
    DefaultTTL   uint32         `json:"defaultTTL,omitempty"`     // 300
    Policy       ZonePolicy     `json:"policy"`                   // Technitium's six-way vocabulary [technitium §4.2]
    Forward      *ForwardSpec   `json:"forward,omitempty"`        // upstreamGroup, dnssecValidation
    Secondary    *SecondarySpec `json:"secondary,omitempty"`      // primaries[], tsigKeyRef, transferProtocol tcp|tls (v0.2)
    Stub         *StubSpec      `json:"stub,omitempty"`           // nameservers[] (v0.2)
    External     *ExternalSpec  `json:"external,omitempty"`       // ingress/gateway/loadbalancer sources (v0.2)
    DNSSEC       *DNSSECSpec    `json:"dnssec,omitempty"`         // enabled, algorithm, denial nsec|nsec3, keySecretRef, kskRollover, zskRolloverDays (v1.0; schema reserved)
    RecordSetSelector *RecordSetSelector `json:"recordSetSelector,omitempty"` // namespaces[]/labelSelector allowed to add DNSRecordSets
    Publish      []PublishRule  `json:"publish,omitempty"`        // split-horizon: integrationRef + selector + types + policy + defaults (v0.2)
}
type ZonePolicy struct {
    QueryAccess  AccessMode `json:"queryAccess"`     // deny|allow|allowOnlyPrivate|allowOnlyZoneNameServers|useNetworkACL|zoneNameServersAndACL
    QueryACL     []string   `json:"queryACL,omitempty"`      // "10.0.0.0/8", "!10.0.0.2": ordered, first match wins, default deny
    ZoneTransfer AccessMode `json:"zoneTransfer"`
    TransferACL  []string   `json:"transferACL,omitempty"`
    TransferTSIGKeyRefs []string `json:"transferTSIGKeyRefs,omitempty"`
    Notify       NotifyMode `json:"notify"`          // none|zoneNameServers|specified|both
    NotifyTargets []string  `json:"notifyTargets,omitempty"`
    Update       AccessMode `json:"update"`          // RFC 2136 (v0.2)
    UpdatePolicies []UpdatePolicy `json:"updatePolicies,omitempty"` // {tsigKeyRef, name, types[]}
}
type DNSZoneStatus struct {
    ObservedGeneration  int64              `json:"observedGeneration"`
    Serial              uint32             `json:"serial"`             // stamped by the leader = max(prev+1, unix_now); replicas serve this value
    ContentHash         string             `json:"contentHash"`        // sha256 of canonical RRsets; drives the serial-divergence panel
    RecordCount         int                `json:"recordCount"`
    NameserverAddresses []string           `json:"nameserverAddresses,omitempty"` // auto-detected LB IPs (k8s_gateway idea)
    DNSSEC              *DNSSECStatus      `json:"dnssec,omitempty"`   // ds[], keys[]{id,type,state}, nextResign (v1.0)
    Published           []PublicationStatus `json:"published,omitempty"` // per integration: managed, drift, conflicts, lastSync (v0.2)
    Conditions          []metav1.Condition `json:"conditions"`        // Ready, Transferred, Signed, Published, Degraded
}

// api/v1alpha1/dnsrecordset_types.go (namespaced; the "clay copies" app teams own)
type DNSRecordSetSpec struct {
    ZoneRef   ObjectRef  `json:"zoneRef"`                 // immutable
    Endpoints []Endpoint `json:"endpoints"`               // ≤ 1000 per object (CRD size ceiling), many objects per zone
}
type Endpoint struct {                                    // superset of sigs.k8s.io/external-dns/endpoint.Endpoint; ToExternalDNS() drops Kudurru
    DNSName          string             `json:"dnsName"`
    RecordType       string             `json:"recordType"`   // A AAAA CNAME DNAME MX NAPTR NS PTR SRV TLSA TXT CAA HTTPS SVCB SSHFP + TYPEnnn (RFC 3597)
    Targets          []string           `json:"targets"`      // presentation-format rdata
    RecordTTL        int64              `json:"recordTTL,omitempty"`
    SetIdentifier    string             `json:"setIdentifier,omitempty"`
    Labels           map[string]string  `json:"labels,omitempty"`        // e.g. kudurru.io/publish: cloudflare
    ProviderSpecific []ProviderSpecific `json:"providerSpecific,omitempty"` // cloudflare/proxied, cloudflare/targets …
    Kudurru          *EndpointExt       `json:"kudurru,omitempty"`       // comment, disabled, expiresAt, view (public | <ClientGroup>) (view v0.2)
}
```

Other kinds (abbreviated; full schemas are generated into `deploy/helm/kudurru-crds`):

| Kind (scope) | Spec essentials | Status | CRD |
|---|---|---|---|
| `BlockList` (ns) | `sources[]{url\|inline\|configMapRef, format auto\|hosts\|domains\|adblock\|regex, subdomains, enabled}`, `mode block\|allow`, `refreshInterval`, `response nxdomain\|zeroIP\|refused\|customIP{v4,v6}` | `Loaded{entries,ruleCount,invalid,etag,lastFetched}`, `sources[].digest`, `Ready` | v0.1 |
| `ClientGroup` (ns) | `clients[]{ip\|cidr\|mac(v0.2)\|clientID(v0.2)\|hostname\|nameGlob, name, comment}`, `sources{integrationRef}` (v0.2) | `Ready`, `members` | v0.1 |
| `Policy` (ns) | `clientGroupRefs[]`, `blockListRefs[]`, `allowListRefs[]`, `allowOnly`, `schedule{weekdays,start,end,tz}`, `upstreamGroup`, `blockMode`, `blockTTL`, `queryLog`, `priority`, `rewrites[]` (v0.2), `safeSearch` (v0.2) | `Ready` | v0.1 |
| `Kudurru` (cluster singleton `default`) | cluster-wide settings: upstream groups, cache, blocking defaults, querylog defaults, auth providers, edge/vip defaults, feature flags | `Ready`, `Listening`, `UpstreamsHealthy`, `blocking{disabledUntil}` | YAML/ConfigMap v0.1 (spec); the writable runtime status field lives in the `kudurru-runtime` ConfigMap in v0.1 k8s (see §7.1 step 7); CRD v0.2 |
| `Integration` (ns) | `type`, `credentials{…secretRef\|file}`, one-of `cloudflare{}`/`unifi{}`/`webhook{}`, `resync`, `suspend`, `deletionPolicy` | `Ready`, `Authenticated`, `Synced`, `Degraded`, counters | YAML v0.1 (framework), CRD v0.2 |
| `IdentityProvider` (cluster) | issuer, clientID, clientSecretRef, cliClientID, scopes, audiences, claimValidationRules, claimMappings, provisioning, allowedDomains | `Discovered` | YAML v0.1; CRD v0.2 |
| `RoleBinding` (ns) | `role`, `scope{kind cluster\|zone\|zonegroup\|clientgroup, name}`, `subjects[]` | — | YAML v0.1; CRD v0.2 |
| `ServiceAccount` (ns) | description, token{secretName, expiresAfter} | tokenID, lastUsed | YAML v0.1; CRD v0.2 |
| internal kinds (never CRDs; Secrets in k8s, tables in SQLite) | `LocalUser` (Argon2id hash, TOTP secret), `APIToken` (sha256), `TSIGKey`, `DNSSECKey`, `IntegrationLedger`; **all secret material in these kinds (TSIG secrets, DNSSEC private keys, TOTP seeds, encrypted refresh tokens in v0.2) is AES-256-GCM-encrypted with the cluster encryption key before it reaches `state.db`/`local.db`**; in k8s they are `Secret`s and etcd encryption-at-rest is the cluster's concern | — | — |

Validation is CRD OpenAPI + CEL first (bounded lists, immutability, `has(self.cloudflare) == (self.type == 'cloudflare')`); the admission webhook is optional (`webhook.enabled=false`) so a basic install needs no cert-manager [k8s §7.3]. Cross-object checks (record under its zone, duplicates across namespaces) are reconciler conditions (`OutsideZone`, `Duplicated`), PowerDNS-Operator style.

Example (both modes; `kudurru apply -f` accepts this verbatim):

```yaml
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home-example-com }
spec:
  zoneName: home.example.com.
  type: primary
  soa: { mname: ns1.home.example.com., rname: hostmaster.example.com., refresh: 3600, retry: 900, expire: 1209600, minimum: 300 }
  nameservers: [ns1.home.example.com., ns2.home.example.com.]
  policy: { queryAccess: allow, zoneTransfer: useNetworkACL, transferACL: ["10.0.0.0/8"], notify: zoneNameServers, update: deny }
  recordSetSelector: { namespaces: [dns, apps] }
  publish:                                           # v0.2 split-horizon
  - integrationRef: { name: cf-public, namespace: dns }
    selector: { matchLabels: { kudurru.io/publish: cloudflare } }
    types: [A, AAAA, CNAME, TXT, MX, SRV, CAA]
    policy: sync
status:
  serial: 1791234567
  recordCount: 42
  nameserverAddresses: [10.0.0.53]
  conditions:
  - { type: Ready, status: "True", reason: Serving, observedGeneration: 3 }
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: nas, namespace: apps, labels: { kudurru.io/publish: cloudflare } }
spec:
  zoneRef: { name: home-example-com }
  endpoints:
  - { dnsName: nas.home.example.com, recordType: A, targets: ["10.0.0.20"], recordTTL: 300, kudurru: { comment: "Synology" } }
  - { dnsName: nas.home.example.com, recordType: HTTPS, targets: ["1 . alpn=h2,h3"] }
  - dnsName: nas.home.example.com                     # v0.2: published to Cloudflare, never served internally
    recordType: CNAME
    targets: [tunnel-7f3a.cfargotunnel.com]
    providerSpecific: [{ name: cloudflare/proxied, value: "true" }]
    kudurru: { view: public }
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: { name: default-ads, namespace: dns }
spec: { mode: block, sources: [{ url: https://big.oisd.nl/, format: auto }], refreshInterval: 24h }
---
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: { name: kids, namespace: dns }
spec: { clients: [{ cidr: 10.0.20.0/24, name: "Kids VLAN" }, { hostname: "ps5*" }] }
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: { name: kids, namespace: dns }
spec: { clientGroupRefs: [kids], blockListRefs: [default-ads, adult], blockMode: zeroIP, blockTTL: 10m, schedule: { weekdays: [mon,tue,wed,thu,fri], start: "08:00", end: "20:00", tz: Europe/Berlin }, priority: 10 }
```

### 4.2 Record semantics the comparables got wrong

- Zones have **types and SOA/serial management** from v0.1 (Gravity hard-codes serial 1337 [gravity §18.6]). Serial rule everywhere: `status.serial = max(prev+1, unix_now)`, stamped by the leader whenever the zone's `contentHash` changes (kube driver: leader stamps on informer events; sqlite driver: in the same transaction). Replicas serve `status.serial`; the window where a replica has newer content than the stamped serial is bounded by lease renewal (≤ 15 s) and only matters to external secondaries, which the leader NOTIFYs after stamping [k8s §8].
- Typed RRsets in presentation format; all common types incl. SVCB/HTTPS (browsers bypass overrides without it [gravity #1335]), CAA, TLSA, SSHFP, NAPTR; unknown types as RFC 3597.
- CNAME exclusivity and duplicate detection across namespaces (`Duplicated` on the later object); `mergeStrategy: union` opt-in.
- Records never carry secrets. `Labels`, `ProviderSpecific` and `kudurru{comment,disabled,expiresAt,view}` are the only extension points; split-horizon overrides live in `providerSpecific` (`cloudflare/targets`, `cloudflare/proxied`) and in `kudurru.view` [integrations §2.5].
- Per-record `disabled` and `expiresAt` (Technitium's expiry TTL) are honoured by `zonecache`, not by the store.

### 4.3 The Store interface (the seam everything depends on)

```go
// internal/store/store.go
type Key struct{ Kind Kind; Namespace, Name string }
type Store interface {
    Get(ctx context.Context, key Key, into Object) error
    List(ctx context.Context, kind Kind, opts ListOptions) (ObjectList, error)   // keyset pagination, label selectors
    Apply(ctx context.Context, obj Object, opts ApplyOptions) (Object, error)    // whole-object apply; see contract below
    Delete(ctx context.Context, key Key, opts DeleteOptions) error              // honours finalizers
    UpdateStatus(ctx context.Context, obj Object) error                          // leader-only callers
    Watch(ctx context.Context, kind Kind, opts WatchOptions) (Watcher, error)    // list+watch, resync, compaction-safe; Added|Modified|Deleted|Bookmark
    Elector(name string) Elector                                                 // leader election primitive (named: controller, integration-<ns>-<name>)
    Snapshot(ctx context.Context, w io.Writer, opts SnapshotOptions) error       // `kind: List` YAML of all kinds (+ internal kinds unless --safe)
    Restore(ctx context.Context, r io.Reader) error                              // = apply
    Info() Info                                                                  // driver, SchemaVersion, ClusterID, Epoch, Leader
}
type ApplyOptions struct {
    Manager                 string   // "api" | "file:<path>" | "integration:<name>" | "bootstrap" | "system"  → metadata.managedBy
    ExpectedResourceVersion string   // optimistic concurrency; mismatch → ErrConflict (409 conflict)
    Force                   bool     // override managedBy (admin only, audited)
}
type Elector interface { Run(ctx context.Context, onStarted, onStopped func()) error; IsLeader() bool; Leader() string }
```

**Apply contract (bounds the "SSA emulation" to something a small team can keep correct):**

1. Whole-object apply; no partial KV puts. The API layer computes merges for `PATCH` and `records` convenience routes and then calls `Apply`.
2. `metadata.managedBy` must equal `opts.Manager` or be empty (first write); otherwise `ErrManagedBy{Owner}` → HTTP `409 managed_by` with the owner in `hint` (UI shows "managed by file objects/zones.yaml" / "managed by Kubernetes"). In Kubernetes the kube driver uses server-side apply with `FieldOwner(manager)` and `ForceOwnership` only when `Force`; `managedBy` is mirrored in the annotation `kudurru.io/managed-by`. Field-manager tracking below the whole object is **not** emulated in the sqlite driver — by design.
3. The caller (API/controller) stamps `uid`, `creationTimestamp`, timestamps and serial candidates; drivers store them verbatim (the apiserver necessarily overrides `uid`/`resourceVersion`/`creationTimestamp` in k8s). This keeps the future raft FSM deterministic without touching callers.
4. Validation (OpenAPI + CEL, bounded lists, immutability) runs in the driver-independent `store.Validator` **before** any driver; both modes reject the same inputs. Entry points (D70): `Validate` for Apply, `ValidateStatus` for UpdateStatus, `ValidateUnstructured` for API and file input (strict unknown fields, complete defaulting, then conversion to the Go type). The validator writes schema defaults back into the object. Every list whose items carry CEL rules needs `maxItems` or the CRD will not install (the apiserver's static cost estimate; `TestEmbeddedCRDsInstall` enforces it); optional struct-typed fields with defaults use `omitzero`.
6. Status and generation (D70): Apply writes the main resource and ignores `status`; `UpdateStatus` writes status and ignores `spec`; `generation` increments only when the spec changes and an identical re-apply is a no-op (resourceVersion may stay). `Force` is a whole-object takeover (field ownership reassigned first).
5. Audit: the audit row for a mutation is written in the same SQLite transaction as the mutation in the sqlite driver (table `audit` in `state.db`); the kube driver keeps none, so the process wraps it in `audit.WrapStore`, which records every successful Apply and Delete (status writes are not recorded; internal kinds without documents) to the node's audit spool (`audit-spool.db`) and the `"log":"audit"` lines after the apiserver accepted the write (D156); every node ships its spool into `AuditLog` batches that every replica reads (`audit.store: auditlog`, the default with the kube driver, D157); the apiserver audit log covers the write as well.

| Driver | Backing | Used in | Watch | Elector | Status |
|---|---|---|---|---|---|
| `kube` | controller-runtime client + informer cache over CRDs/Secrets; **`local.db` last-good snapshot** on disk | Kubernetes (any replica count) | informers | `coordination.k8s.io/Lease` 15 s/10 s/2 s | **v0.1** |
| `sqlite` | `state.db` (modernc): objects as JSON rows + `generation`/`resourceVersion` columns + change-feed table; `audit` table | `docker run`; k8s "standalone" profile | in-process bus + resync | always leader | **v0.1** |
| `follower` | `sqlite` mirror + periodic pull of the primary's `Snapshot` (ETag/`ifModifiedSince`) + push NOTIFY from the primary; writes forwarded to the primary; fenced promote | compose HA before raft | as sqlite | never leader (primary is) | **v0.2** |
| `raft` | hashicorp/raft log (raft-boltdb), SQLite FSM, single-voter bootstrap, learner join via API token, snapshot before membership change | compose/bare-metal HA, multi-primary | FSM apply hook | raft leadership | **v1.0** |

**Schema versioning (every driver, from v0.1):** `store.SchemaVersion` is compiled into the binary; `state.db`/`local.db` carry `meta.schema_version`. On open: a newer on-disk version than the binary → refuse to start loudly (`doctor` explains); older → migrate inside one transaction. Follower (v0.2): the primary serves snapshots with `X-Kudurru-Schema-Version`; a follower at a lower version halts replication with a diagnosis message instead of guessing. Raft (v1.0): every `Command` carries `MinVersion`; cluster version = min(node versions); the leader applies `Migrate` only when every member is at or above the target.

Rules that keep the drivers interchangeable: serving never reads the store on the query path; runtime data (query log, cache, sessions) never goes through `Store`; backup format = `Snapshot` output = the YAML `kudurru apply -f` accepts = `kubectl get kudurru.io -A -o yaml` (`kind: List`); the **conformance suite** (`internal/store/conformance`) runs against `sqlite` and `kube` (envtest) in CI and later against `follower` and `raft`.

### 4.4 Exact HA decision per deployment shape

| Shape | Config truth | Replicas | Leader | HA of DNS answers | HA of config writes | Version |
|---|---|---|---|---|---|---|
| **Single Docker** | `state.db` on a volume | 1 | self | none; optional dnsdist for transports/cache | n/a | v0.1 |
| **Kubernetes multi-replica** | CRDs (apiserver/etcd) | Deployment ≥ 2, PDB, topology spread | Lease | every pod serves from informer snapshots; keeps serving from memory when the apiserver is away; a restarted/rescheduled pod serves from `local.db` while waiting for the apiserver (below) | apiserver HA; UI/API writes go through the apiserver | v0.1 |
| **Kubernetes standalone** | `state.db` on a PVC | StatefulSet 1 | self | none | n/a ("Pi-hole on k3s", no CRDs) | v0.1 |
| **Compose HA, static twins** (`ha-static`) | identical file-managed `kudurru.yaml` + `objects.d/` on every node (replicated by the operator's own tooling: git, ansible) | N nodes; every node's dnsdist lists all nodes; keepalived VIP | self on each node | all nodes answer | file-managed only; `cluster.role: static` puts the API/UI in **read-only mode for all `kudurru.io` objects** (not just file-managed ones), so twins cannot diverge through node-local `managedBy: api` writes; runtime actions (blocking toggle, cache flush) are node-local and badged as such | v0.1 |
| **Compose HA, follower mode** | primary's `state.db` | 1 primary + N followers; dnsdist + keepalived as above | primary | all nodes answer; followers serve a ≤ `pull` seconds old config and current AXFR/IXFR copies | primary only; writes on a follower are forwarded; **fenced** `kudurru cluster promote` | v0.2 |
| **Compose HA, raft** | replicated `state.db` | 3 or 5 voters (+ nonvoters) | raft | all nodes | any node (forwarded to leader) | v1.0 |

**Cold start without the apiserver .** `zonecache` writes the complete materialised object set (all `kudurru.io` kinds as JSON + the `resourceVersion` high-water mark) to `local.db` after every successful sync and then at most every 30 s when changed. On start, if informers have not synced within `store.kube.startupTimeout` (15 s), the pod loads `local.db`, sets `Degraded{reason: ServingFromSnapshot}`, exposes `kudurru_store_snapshot_serving 1` and `kudurru_store_snapshot_age_seconds`, and reports **ready** (so it takes traffic: cached and forwarded answers and primary zones from the snapshot). When informers sync, the snapshot is swapped out. If the snapshot is older than `store.kube.snapshot.maxAge` (24 h), primary zones answer SERVFAIL (never NXDOMAIN) while forwarding continues. `local.db` lives on `emptyDir` by default (survives container restarts and node reboots while the pod stays assigned) and on a PVC with `persistence.local.enabled` for clusters that need DNS before their control plane. With `priorityClassName: system-cluster-critical` and `dnsPolicy: Default` this closes the "DNS needed to bring the cluster up" trap.

**Fenced promotion (follower mode, v0.2).** `state.db.meta` carries `cluster_epoch` and `primary_id`. `kudurru cluster promote` on a follower increments the epoch and records itself as primary; every node advertises `{epoch, primary_id}` on `/internal/status`; a former primary that reconnects and sees a higher epoch demotes itself to follower of the new primary, refuses writes (`503 fenced`), and raises `kudurru_cluster_fenced 1` + an alert. The UI only offers "Promote" when the old primary has been unreachable for ≥ 2 pull intervals and requires typing the node name; the residual split-brain window exists only while partitioned **and** promoted — documented, and the reason raft replaces this in v1.0.

Why not embedded etcd or a Raft object-store driver in v0.1: the k8s path already has a replicated store; the single-Docker path needs none; writing membership/join/snapshot/mixed-version logic is where Gravity accumulated its worst bugs (#837, #894, #1093, #1389, #927, #1949) [gravity §16]; we never have two editable sources of truth [k8s §16.1]. The raft driver adopts the single-binary-first proposal's safety rules up front: a node with existing raft state ignores bootstrap settings; a failed join retries forever with backoff and **never** bootstraps a new cluster; `doctor` warns on 2-node quorum; hostnames are raft addresses; `preStop` runs `kudurru cluster leave`.

DHCPv4 quorum failover uses a separate Raft group in v0.1.0 (D176/D179);
it replicates leases and live DHCP management independently of the DNS
object store. See [DHCP failover](../docs/architecture/dhcp.md#three-node-quorum-failover).

### 4.5 Runtime data placement

| Data | Docker | Kubernetes | Replicated? |
|---|---|---|---|
| Query log, rollups | `/var/lib/kudurru/querylog.db` | `emptyDir` (default) or `persistence.querylog` PVC per pod; never RWX/NFS (WAL) | no (scatter-gather) |
| Audit | `audit` table in `state.db` (atomic with mutations); with `database.dsn` also shipped to the central database (D159) | `audit-spool.db` per pod (hash-chained, capped at 50 000 events, D156), shipped into the central database with `database.dsn` (D159), else into immutable `AuditLog` batches in the release namespace (D157) + stdout JSON | yes in Kubernetes (`AuditLog` objects or the central database); in Docker with `database.dsn` |
| Last-good config snapshot | n/a (`state.db` is local) | `local.db` | no |
| DHCPv4 leases and managed options (v0.1.0) | `dhcp.db` or `dhcp-<scope>.db`, local SQLite plus DNS claim registry; optional three-voter `dhcp-raft` | `DHCPLease` objects in `dhcp.kudurru.io` (D180); a per-pod working copy under `dhcp-kube/`, rebuilt from them | SQLite: no, preserve the complete node directory; Kubernetes: yes, in the apiserver (not in backups) |
| Response cache, compiled lists | RAM; list disk cache under `lists/` | RAM; `emptyDir` | no |
| Sessions | sealed cookie (v0.1); `db` store in `state.db` (v0.2) | sealed cookie | n/a |
| Node status / heartbeats | **nowhere** — scatter-gather of `/internal/status` | **nowhere** — EndpointSlice + scatter-gather | no |
| API token hashes, local users, TSIG, DNSSEC keys, ledgers | tables in `state.db` | `Secret`s labelled `kudurru.io/kind=<internal>` via the kube driver | yes (by the store) |
| Cookie/encryption key | `KUDURRU_ENCRYPTION_KEY[_FILE]`; rotation via `KUDURRU_ENCRYPTION_KEY_PREVIOUS[_FILE]` (decrypt with either, re-encrypt stored secrets on start, sessions sealed under the old key stay valid until `lifetime`) | chart-generated `Secret` (`helm.sh/resource-policy: keep`); same `_PREVIOUS` rotation | shared |
| Internal listener token + node TLS cert/CA | HKDF from the encryption key (override: `listeners.internal.tokenFile`/`tls`) | same derivation; no extra Secret | derived, not stored |

### 4.6 Leader-only work and what happens on failover

Leader loops are idempotent and level-triggered; timers they need to resume (`nextResign`, secondary refresh, list refresh, resync) are stored in object `status`, so any new leader resumes without redoing work. Specific rules:

- **Blocklists:** leader fetches, publishes `status.sources[].digest`; replicas pull bodies from the leader's `:8082/internal/lists/<digest>` (sha256-verified) and compile locally; after 10 min without a reachable leader a replica falls back to its disk cache, then to fetching itself (§7.3).
- **Secondary zones (v0.2):** every replica transfers independently from the configured external primaries and persists the zone in `local.db` (no leader in the transfer path, so leader flaps cannot re-transfer); NOTIFY-in arrives at whichever pod the LB picks and is fanned out over `:8082/internal/events`; the leader only reports `Transferred`.
- **Signed zones (v1.0):** the leader signs; signed RRsets and the journal are zone data (never CRDs) held in the leader's `local.db` and served to replicas by **IXFR over the headless Service with a chart-generated TSIG key**; replicas persist them. A **new leader does not re-sign on takeover**: it loads the current signed state by IXFR from any replica (or its own copy), reads keys from the Secret and continues the schedule in `status.dnssec.nextResign`; it signs only RRsets whose content changed or whose RRSIGs are within the resign window. ECDSA non-determinism is therefore irrelevant (one signer at a time, sequential) [go-dns-stack §3.2, §17.4].
- **Integrations (v0.2):** one `Lease` per instance so instances spread across replicas and fail over independently.
- **NOTIFY/AXFR towards external secondaries from Kubernetes:** NOTIFY leaves from the leader pod and is SNAT'ed to a node/egress IP on most CNIs and clouds, and the secondary's `allow-notify`/`primaries` list must contain that address, not the Service VIP; `kudurru doctor` reports the observed egress address (via a self-NOTIFY round trip to the `health.kudurru.internal.` listener) and the chart documents an egress gateway / `hostNetwork` DaemonSet as the fix when a stable source is required. AXFR/IXFR *to* kudurru (secondaries, v0.2) and *from* kudurru (TCP via the LoadBalancer) are unaffected.

---

## 5. Kubernetes model

"Kubernetes-native" means three separable things [k8s §1]; kudurru does all three, staged:

1. **Good-citizen dataplane (v0.1):** non-root on :53 under the Restricted PSS, `/healthz/*` split, lameduck, Deployment + PDB + topology spread, Helm OCI + kustomize, `dnsPolicy: Default`.
2. **Kubernetes API as configuration surface (v0.1 for DNS/blocking kinds, v0.2 for IAM/Integration/Kudurru):** CRDs reconciled by a Lease-elected controller inside the same binary; `status.conditions` per api-conventions; `Events`; never `phase`.
3. **Kubernetes as a DNS data source (v0.2):** `DNSZone.type: external` synthesises records from Ingress / Gateway API routes / LoadBalancer Services via stripped, indexed informers (k8s_gateway / CoreDNS technique), honouring `external-dns.kubernetes.io/{hostname,target,ttl}` (+ legacy `alpha` prefix). Replacing CoreDNS for `cluster.local` is **not** in scope before 1.x (§15).

### 5.1 Objects shipped

- CRD chart `kudurru-crds` (Helm never upgrades `crds/`; separate chart + `helm.sh/resource-policy: keep`; docs show `kubectl apply --server-side -f crds.yaml`) [k8s §9.5].
- v0.1 CRDs: `DNSZone` (cluster), `DNSRecordSet`, `BlockList`, `ClientGroup`, `Policy` (namespaced); `AuditLog` (`audit.kudurru.io`, namespaced, runtime data the store never serves, D157); compatibility watch of `externaldns.k8s.io/v1alpha1 DNSEndpoint` (zone inferred by suffix, read-only, never owned).
- v0.2 CRDs: `Kudurru`, `Integration`, `IdentityProvider`, `RoleBinding`, `ServiceAccount`. Typed Secrets: `kudurru.io/tsig`, `kudurru.io/dnssec-key`, `kudurru.io/session-key`.
- Aggregated ClusterRoles `kudurru-viewer|editor|admin` labeled `rbac.authorization.k8s.io/aggregate-to-view|edit|admin` (PowerDNS-Operator pattern) plus `kudurru-auditor`, which aggregates nowhere because it reads the query log; `make rbac-manifests` generates them from `internal/rbac/permissions.yaml` into `deploy/helm/kudurru/templates/rbac-aggregated.yaml` and a test fails on drift [k8s §10].
- Print columns and short names: `kubectl get zones` → `ZONE TYPE SERIAL RECORDS READY AGE`; `rrsets` → `ZONE NAMES READY PUBLISHED AGE`; `blocklists` → `MODE ENTRIES LAST-FETCH READY`; shortNames `zone, rrset, blocklist, cg, pol, integ, idp`.
- Events on CRs: `SerialBumped`, `NotifyFailed`, `ListFetchFailed`, `Duplicated`, `OutsideZone`, `PublishConflict`, `Transferred`, `Signed`.
- API versions `v1alpha1` → `v1beta1` (v1.0 release) → `v1`; storage-version progression with no-op conversion until a breaking change forces a webhook.

### 5.2 Workload, Helm values outline, probes, ports

Chart `deploy/helm/kudurru` (values modeled on the CoreDNS chart):

```yaml
mode: kubernetes                    # kubernetes = kube driver + Deployment | standalone = sqlite driver + StatefulSet(1) + PVC, no CRDs
replicaCount: 2
image: { repository: ghcr.io/robertherbaugh/kudurru, tag: "", digest: "", pullPolicy: IfNotPresent }
service:
  dns:  { type: LoadBalancer, port: 53, splitProtocols: false, loadBalancerIP: "", loadBalancerClass: "",
          externalTrafficPolicy: Local, ipFamilyPolicy: PreferDualStack, trafficDistribution: "", annotations: {} }
        # one mixed-protocol Service by default (GA since 1.26); splitProtocols=true emits dns-udp + dns-tcp with metallb.io/allow-shared-ip
  dot:  { enabled: false, port: 853 }
  doh:  { enabled: false, port: 443 }
  doq:  { enabled: false, port: 853 }             # udp
  doh3: { enabled: false, port: 443 }             # udp
  api:  { type: ClusterIP, port: 8080 }           # expose via Ingress/HTTPRoute; OIDC redirect URL derived from ingress.host
  headless: { name: kudurru-dns, publishNotReadyAddresses: true }   # edge backends, IXFR between replicas, NOTIFY source
ingress: { enabled: false, className: "", host: "", tls: [] }
listen: { port: 53 }                # 1053 escape hatch for clusters that forbid NET_BIND_SERVICE
edge:                               # dnsdist tier (v0.1 behind a flag; default off)
  enabled: false
  mode: deployment                  # deployment | sidecar (single-node k8s: shares the pod netns, kudurru on 5353) | daemonset (with vip.provider=keepalived)
  replicas: 2
  image: { repository: ghcr.io/robertherbaugh/kudurru-dnsdist, digest: "" }
  authoritativeCache: false
  dynblocks: { enabled: false }
  ebpf: { enabled: false }          # opt-in "performance" profile; needs retained caps
  proxyProtocolSecret: ""           # generated once if empty; mounted in edge AND kudurru pods; PROXYv2 TLV 0xE0 (§3.3) — pod-CIDR trust alone is spoofable
vip:
  provider: none                    # none | kube-vip | metallb | keepalived
  address: ""                       # emits kube-vip.io/loadbalancerIPs or metallb.io/loadBalancerIPs (+ allow-shared-ip when split)
  keepalived: { nodeSelector: { kudurru.io/vip: "true" }, interface: "", vrid: 53, hmacSecret: "" }  # opt-in DaemonSet + hostNetwork edge
hostNetwork: { enabled: false }     # switches to DaemonSet, NET_BIND_SERVICE, dnsPolicy ClusterFirstWithHostNet
securityContext: { runAsNonRoot: true, runAsUser: 65532, readOnlyRootFilesystem: true, allowPrivilegeEscalation: false,
                   seccompProfile: { type: RuntimeDefault }, capabilities: { drop: [ALL], add: [NET_BIND_SERVICE] } }
resources: { requests: { cpu: 100m, memory: 256Mi }, limits: { memory: 1Gi } }
podDisruptionBudget: { enabled: true, minAvailable: 1 }
topologySpreadConstraints: [ { topologyKey: kubernetes.io/hostname, whenUnsatisfiable: ScheduleAnyway } ]
priorityClassName: ""               # system-cluster-critical when kudurru is the cluster resolver
dnsPolicy: Default                  # never ClusterFirst for the DNS server itself
persistence:
  querylog: { enabled: false, storageClass: "", size: 10Gi, accessModes: [ReadWriteOnce] }
  local:    { enabled: false, size: 1Gi }         # local.db on a PVC for "DNS before control plane" clusters
  standalone: { size: 5Gi }                       # mode=standalone only
config:                             # rendered into TWO ConfigMaps: kudurru-node (hostOnly/restartRequired keys → checksum annotation rolls pods)
  node: {}                          #                             kudurru-settings (hot-reloaded on change, no restart)
  settings: {}                      # the Kudurru singleton (v0.1 ConfigMap; v0.2 CRD)
objects: []                         # optional inline kudurru.io objects applied by a post-install Job (seed only; then kubectl owns)
auth:
  encryptionKeySecret: ""           # generated once if empty
  bootstrapAdminSecret: ""          # generated once if empty; printed by `helm status` notes
  oidc: { issuer: "", clientID: "", clientSecretRef: {}, groupsClaim: groups, groupsPrefix: "corp:" }
  kubernetes: { mode: sar }         # sar (default) | impersonate (v0.2, needs the impersonate ClusterRole and auth.kubernetes.usernamePrefix/groupsPrefix)
rbac: { create: true, aggregateToDefaultRoles: true }
webhook: { enabled: false }         # validating webhook optional; CEL does the rest
serviceMonitor: { enabled: false }  prometheusRule: { enabled: false }  grafanaDashboard: { enabled: false }
networkPolicy: { enabled: false }
externalDNS: { webhook: { enabled: false, listen: ":8888" } }   # kudurru as an external-dns webhook provider (in-process) or sidecar values
```

The shipped chart differs from this outline where milestone M3 decided so (D105): node and settings files at `/etc/kudurru/node/` and `/etc/kudurru/settings/`, `preStop: sleep` until a server-side `POST /lameduck` exists (the client is `kudurru lameduck 5s`), the `ip_unprivileged_port_start` sysctl instead of a file capability, a StatefulSet for per-pod persistence, keepalived as a second container of the edge DaemonSet, and `auth.oidc.providers` (a list rendered into `auth.identityProviders`) instead of the single-issuer `auth.oidc`; docs/deploy/helm.md lists every value.

Pod template facts: image `ghcr.io/robertherbaugh/kudurru:<ver>@sha256` (distroless static, uid 65532, file capability `cap_net_bind_service=+p` raised only when a port < 1024 is configured, blocky-style) [k8s §9.2]; `checksum/config` annotation covers **only** the `kudurru-node` ConfigMap (CRD and settings changes never restart pods) [k8s §16.6]; `terminationGracePeriodSeconds: 30` with `preStop: kudurru lameduck 5s`; args `serve --store=kube --node-name=$(POD_NAME) --proxy-protocol-from=<edge pod CIDR>` (a CIDR is accepted only together with the TLV secret, D72).

Probes on the ops listener `:8081` (no auth) [observability §4.1]:

| Probe | Endpoint | Semantics |
|---|---|---|
| startup | `GET /healthz/startup` | config parsed, store opened (informers synced **or** `local.db` loaded), listeners bound; `failureThreshold` sized for list compile (60 × 5 s) |
| readiness | `GET /healthz/ready` | all zone watches synced (or serving from a snapshot younger than `maxAge`), blocklists loaded or `blocking.allowDegraded`, loopback self-query `health.kudurru.internal. A` ≤ 2 s; **upstream reachability is never a readiness condition**; 503 during lameduck |
| liveness | `GET /healthz/live` | self-check loop completed within 30 s; never load-based |
| human | `GET /healthz` | JSON roll-up `{status, components, version, node, leader, snapshotAge}`; `kudurru health` and Docker `HEALTHCHECK` |

Ports: 53/udp+tcp DNS (5353 PROXYv2-only when the edge is enabled), 853/tcp DoT, 853/udp DoQ, 443/tcp DoH, 443/udp DoH3 (8443 if not root-capable), 8080 API/UI, 8081 ops, 9153 metrics, 8082 internal (node-to-node), 8084 edge supervisor readiness, 8888 external-dns webhook (optional), 7000/7001 raft + peer API (v1.0).

### 5.3 Leader election, leases, RBAC for the kudurru ServiceAccount

Lease `kudurru-controller` in the release namespace, 15 s/10 s/2 s, identity = pod name; runnables implement `NeedLeaderElection()` per §3.1; per-integration Leases `integration-<ns>-<name>` (v0.2); `kudurru_leader{role}` gauge; **no per-node Lease and no NodeStatus objects** — the Nodes page scatter-gathers `/internal/status` over the headless EndpointSlice [observability §5].

**Election lifecycle is decoupled from process lifetime (D44).** The election is driven by `internal/cluster` with client-go's `leaderelection.LeaderElector` directly, *not* by the controller-runtime Manager's process-level election: `OnStartedLeading` starts the leader-only runnable group under a child context, `OnStoppedLeading` cancels only that group and re-enters `Run` after `RetryPeriod`; the process **never exits on leadership loss**. Stock controller-runtime stops the whole Manager when the lease is lost and kubebuilder scaffolds exit the binary (`LeaderElectionReleaseOnCancel` "requires the binary to immediately end when the Manager is stopped" [k8s §7.4]) — that would kill the DNS dataplane on every pod during exactly the apiserver outage §4.4 is designed to survive (lease renewal fails after `RenewDeadline` 10 s). `ReleaseOnCancel` semantics are applied only on SIGTERM (`kudurru lameduck`). A leader that loses the lease while a leader-only loop is mid-operation relies on the idempotent, level-triggered rules of §4.6; the dataplane's informers keep serving the last snapshot throughout.

Least-privilege ClusterRole for the kudurru SA:

```
get,list,watch,create,patch,delete   kudurru.io/* (server-side apply needs create+patch; file objects and finalizers need delete);
get,list,watch            externaldns.k8s.io/dnsendpoints; "" services,namespaces; discovery.k8s.io/endpointslices;
                          networking.k8s.io/ingresses; gateway.networking.k8s.io/httproutes,tlsroutes,grpcroutes (v0.2, only when external zones are enabled)
update,patch              kudurru.io/*/status
create,patch              "" events
get,create,update         coordination.k8s.io/leases   (Elector(name) → Lease `kudurru-<name>`; integrations use `kudurru-integration-<ns>-<name>`, D78)
get,list,watch,create,update,patch,delete   "" secrets   (Role in the release namespace only: internal kinds, TSIG, DNSSEC keys, session key; revocation and user/RoleBinding deletion delete Secrets)
create                    authentication.k8s.io/tokenreviews; authorization.k8s.io/subjectaccessreviews   (system:auth-delegator; v0.1)
impersonate               "" users,groups,serviceaccounts; authentication.k8s.io/userextras/*   (ONLY when auth.kubernetes.mode=impersonate, v0.2)
```

### 5.4 Edge and VIP in Kubernetes

- Default: no dnsdist; the LoadBalancer Service points at kudurru pods; `externalTrafficPolicy: Local` preserves client IPs (per-client stats and client groups are the product) [k8s §16.12].
- `edge.enabled=true` (v0.1): Deployment `kudurru-edge` from the supervisor image with anti-affinity and a PDB; backends from the headless EndpointSlice (`--backends-from endpointslice:<ns>/kudurru-dns`), stable UUID per pod name, `chashed` for the resolver pool, `leastOutstanding` for authoritative, `use_proxy_protocol: true`; readiness probe `GET :8084/healthz/ready` on the supervisor; the Service then targets the edge pods and kudurru listens on 5353 with `proxyProtocolFrom` = the edge pods' CIDR **plus** the `edge.proxyProtocolSecret` TLV check (§3.3) — the pod CIDR is shared with every other workload, so CIDR trust alone would let any pod forge client addresses; the chart's `networkPolicy` template additionally restricts `:5353` ingress to pods labelled `app.kubernetes.io/component=edge`. `edge.mode: sidecar` for single-node k8s shares the pod netns (loopback, no TLV needed) [dnsdist §1.15].
- VIP: `vip.provider` `kube-vip`/`metallb` only add annotations (both Apache-2.0 and Service-aware) [dnsdist §2.5]. `keepalived` deploys a DaemonSet (`hostNetwork`, `NET_ADMIN/NET_RAW/NET_BROADCAST`, nodeSelector `kudurru.io/vip=true`) running `kudurru vip run` with `unicast_peer` rendered from the selected Nodes **and** switches the edge to a hostNetwork DaemonSet on the same nodes so the VIP owner actually listens on :53 [dnsdist §3.3]; re-render + SIGHUP on Node-list change (shared PID namespace not required: the supervisor is PID 1).

### 5.5 Kubernetes identities and authorization (v0.1: SAR; v0.2: impersonation opt-in)

- Bearer tokens that are projected ServiceAccount tokens (`audience: kudurru`) are verified via the cluster's OIDC discovery/JWKS (issuer read from `/.well-known/openid-configuration` with the pod's own token), falling back to `TokenReview` with an audience check, cached ≤ 2 min by token hash; principal `system:serviceaccount:<ns>:<name>` with its groups [auth §4.6].
- `SubjectAccessReview` on `kudurru.io` resources — real CRDs **and virtual resources** (`querylogs`, `stats`, `caches`, `blocking`, `audits`, `clusters`, `settings`) — makes k8s RBAC the policy for Kubernetes identities without any extra CRD; RBAC rules are strings. The kudurru SA needs only `system:auth-delegator`.
- OIDC humans use kudurru `RoleBinding`s (§8.5). **Precedence rule:** a request is allowed if **either** path allows it; the audit record names the path that granted (`authz: sar|binding`).
- `auth.kubernetes.mode: impersonate` (v0.2, opt-in): UI/API writes are sent with `Impersonate-User/Group` headers so apiserver RBAC and audit see the real user. Not the default because the `impersonate` grant makes an internet-facing DNS/HTTP server cluster-admin-equivalent if compromised, and the research never vetted it [k8s §10].

### 5.6 external-dns compatibility

kudurru natively serves the external-dns webhook provider contract (`GET /`, `GET/POST /records`, `POST /adjustendpoints`, `application/external.dns.webhook+json;version=1`, 204 on apply, 5xx-only retries, 32 MiB cap) on `:8888` in-process **on every replica** (reads come from the zone snapshot; applied `Changes` go through `Store.Apply`, which any replica can perform in kube mode because the apiserver is the store — a leader-only listener behind a Service would 503 on non-leader pods) or as a `kudurru externaldns-webhook` sidecar in the external-dns pod; applied `Changes` become `DNSRecordSet`s with `managedBy: integration:external-dns` in a configured namespace; external-dns TXT ownership records are stored as ordinary TXT. `DNSEndpoint` CRs are additionally read directly [k8s §3.4, §3.5].

### 5.7 Packaging

`deploy/helm/kudurru` + `deploy/helm/kudurru-crds` (OCI), `deploy/kustomize/` kubebuilder kustomize tree with overlays `metallb`, `kube-vip`, `cloud-lb-mixed`, `hostnetwork-daemonset`, `standalone`, `with-edge`; release artifacts `crds.yaml` + `install.yaml`; `ServiceMonitor`/`PodMonitor`, Grafana dashboard ConfigMap and `PrometheusRule` (§11.6) behind values. Minimum Kubernetes **1.30**. OLM bundle only after `v1` CRDs.

---

## 6. Single-instance Docker model

### 6.1 Image

`ghcr.io/robertherbaugh/kudurru:<ver>` built by goreleaser `kos:` + ko from `gcr.io/distroless/static:nonroot` (digest pinned), `linux/amd64, linux/arm64` only (D68), cosign keyless signature + SBOM + SLSA provenance. `EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8080 8081 9153`. `HEALTHCHECK CMD ["kudurru","health","--ready"]`. uid 65532; :53 binds through the `net.ipv4.ip_unprivileged_port_start=0` sysctl the chart and Docker provide (D105). Host networking cannot use that sysctl: `<ver>-hostnetwork` (and `latest-hostnetwork`) is the same binary with the file capability `cap_net_bind_service=+ep`, built by publish.yml from the ko image (ko cannot set file capabilities; images/hostnetwork/Dockerfile), and the chart's `hostNetwork.enabled` runs it with `NET_BIND_SERVICE` added and `allowPrivilegeEscalation: false`. Startup contract copied from the dnsdist image: env → rendered config fragment, `KUDURRU_DEBUG_CONFIG=yes` prints the effective config with secrets masked. A `-debug` variant is built on distroless `debug-nonroot`, which adds busybox (a shell and `nslookup`; distroless has no `dig`, D105). Both bases are pinned by digest.

### 6.2 Config file (`/etc/kudurru/kudurru.yaml`) — metadata-driven, two scopes

One metadata table (`internal/config/schema.go`) describes every key: type, default, allowed values, help, flags `restartRequired`, `envLocked`, `writeOnly`, `hostOnly`. It drives the YAML loader (JSON Schema published for `# yaml-language-server`), `KUDURRU_<SECTION>__<KEY>` env overrides and `*_FILE` variants (env-set values become read-only in the UI, Pi-hole's single-source rule), `kudurru config get|set|schema`, `PATCH /api/v1/node/config`, the generated "All settings" page and the docs page; the generator is tested so docs cannot drift [pihole §16.8]. **Code-bearing or privileged keys are `hostOnly` from v0.1** (listener addresses, TLS paths, data dir, `proxyProtocolFrom`, edge/vip paths and VRIDs, cluster peers) and can never be set through the API — the Pi-hole 2026 RCE class made structural [pihole §17.7].

Two scopes in one file: **node** settings (`node`, `listeners`, `data`, `log`, `edge`, `vip`, `cluster`) are host-local; **cluster** settings (`upstreams`, `cache`, `blocking`, `querylog`, `auth`, `rbac`) are the `Kudurru` singleton and are replicated in multi-node shapes. Objects (zones, lists, groups, policies, bindings, integrations) may be embedded under `objects:` or dropped into `objects.d/*.yaml` (file-managed, `managedBy: file:<path>`, re-applied on fsnotify change; deleting a document deletes the object), or live in the store and be edited through UI/API/CLI (`managedBy: api`). `kudurru apply -f` accepts CRD YAML verbatim.

```yaml
# /etc/kudurru/kudurru.yaml  (v0.1)
node: { name: "${HOSTNAME}", dataDir: /var/lib/kudurru }                 # hostOnly
listeners:                                                               # hostOnly
  dns:  [{ address: "0.0.0.0:53", protocols: [udp, tcp] }]
  dot:  [{ address: "0.0.0.0:853", tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  doh:  [{ address: "0.0.0.0:443", path: /dns-query, tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]
  doq:  [{ address: "0.0.0.0:853", tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]                     # udp, ALPN doq
  doh3: [{ address: "0.0.0.0:443", path: /dns-query, tls: { certFile: /certs/tls.crt, keyFile: /certs/tls.key } }]   # udp, ALPN h3
  proxyProtocolFrom: []             # ["127.0.0.1/32","::1/128"] when dnsdist shares the netns; exact /32s, never a LAN /24 (§3.3)
  proxyProtocolSecretFile: ""       # PROXYv2 TLV 0xE0 shared with the dnsdist supervisor; required when proxyProtocolFrom is non-loopback
  api:  { address: "0.0.0.0:8080", tls: { auto: selfsigned }, pathPrefix: "" }
  ops:  { address: "0.0.0.0:8081" }
  metrics: { address: "0.0.0.0:9153" }
  internal: { address: "0.0.0.0:8082", tokenFile: "",                 # token + node cert derived from the encryption key unless set; always TLS
              tls: { certFile: "", keyFile: "", caFile: "", serverName: "" } }   # all empty: the CA and certificate derived from the encryption key
  sockets: 0                        # SO_REUSEPORT sockets per udp/tcp DNS listener; 0 = GOMAXPROCS, 1 = a single socket
upstreams:
  groups:
    default: { servers: ["tls://1.1.1.1", "tls://9.9.9.9"], strategy: parallel_best, timeout: 2s }
    internal-ad: { servers: ["10.0.0.5:53"], strategy: strict }
    private: { mode: recursive, timeout: 4s }   # forward (default) | recursive: no servers, resolves from the root (D170)
  bootstrap: ["1.1.1.1:53"]
  conditional: { "corp.example.": internal-ad, "10.in-addr.arpa.": internal-ad }
  ecs: { mode: strip, ipv4Prefix: 24, ipv6Prefix: 56 }   # strip (privacy default) | forward | add; never sent by recursive groups
  recursion: { dnssec: true, qnameMinimization: true, ipv6: auto, trustAnchors: [], rootHints: [] }   # every recursive group; empty = built-in
cache: { maxEntries: 200000, minTTL: 5s, maxTTL: 24h, negativeTTL: 5m, prefetch: { threshold: 5, window: 2h },
         serveStale: { ttl: 72h, answerTTL: 30s, maxWait: 1800ms } }
blocking:
  enabled: true
  defaultResponse: zeroIP           # zeroIP | nxdomain | customIP | refused (discouraged)
  blockTTL: 10m
  initStrategy: fast                # fast | blocking | failOnError (disk-seeded)
  refreshInterval: 24h
  allowDegraded: true
  listsDir: ""                      # hostOnly; list body cache, default <dataDir>/lists
ratelimit:                          # per-prefix QPM on UDP at ingress (§7.1 step 2); TCP/DoT/DoH/DoQ/DoH3 and loopback never limited
  enabled: true
  ipv4: { "32": 600, "24": 6000 }
  ipv6: { "128": 600, "64": 6000, "56": 12000 }
  slip: 50                          # % of over-limit queries answered TC=1, the rest dropped
  maxBuckets: 65536
  exempt: []
querylog: { enabled: true, privacy: full, retention: { raw: 720h, maxDBBytes: 10GiB }, exclude: { domains: [], clients: [], sudn: true } }
auth:
  localLogin: enabled               # enabled | admins-only | disabled
  bootstrap: { adminPasswordFile: /run/secrets/admin_password }
  session: { idleTimeout: 30m, lifetime: 12h, secure: auto }   # auto = Secure when TLS terminates here or a trusted proxy sends X-Forwarded-Proto: https; plain-HTTP labs still get SameSite=Lax + CSRF header
  identityProviders:
  - id: corp-sso
    issuer: https://auth.example.com/application/o/kudurru/
    clientID: kudurru
    clientSecretFile: /run/secrets/oidc_client_secret
    cliClientID: kudurru-cli
    scopes: [openid, profile, email, groups]
    claimMappings: { username: { claim: preferred_username }, groups: { claim: groups, prefix: "corp:" } }
    provisioning: { autoProvision: mapped-only, syncGroups: true }
  kubernetes: { mode: sar }         # hostOnly; sar (v0.1) | impersonate (v0.2)
  adminSocket: { path: /run/kudurru/kudurru.sock, group: -1 }   # hostOnly; `kudurru admin` (D101); empty path disables
rbac:
  bindings:
  - { role: admin, scope: { kind: cluster }, subjects: [{ kind: Group, name: "corp:/dns-admins" }] }
edge: { enabled: false }            # hostOnly; see compose profiles
vip:  { enabled: false }            # hostOnly
objects:                            # optional inline CRD-shaped objects (managedBy: file:kudurru.yaml)
- apiVersion: kudurru.io/v1alpha1
  kind: BlockList
  metadata: { name: default-ads }
  spec: { mode: block, sources: [{ url: https://big.oisd.nl/ }], refreshInterval: 24h }
- apiVersion: kudurru.io/v1alpha1
  kind: Policy
  metadata: { name: default }
  spec: { clientGroupRefs: [default], blockListRefs: [default-ads] }
```

### 6.3 Persistence under `/var/lib/kudurru`

```
state.db        # sqlite store driver: all api/v1alpha1 objects + internal kinds (JSON rows), change feed, zone journals, audit table; meta.schema_version
querylog.db     # per-node fact table + rollups (WAL, synchronous=NORMAL, incremental_vacuum)
dhcp.db         # default DHCPv4 scope; leases and managed options, WAL/FULL
dhcp-<name>.db  # named DHCPv4 scopes; schema v4 claims and replica progress
.dhcp-dns-claims.sqlite # shared standalone DNS ownership tickets
dhcp-raft/     # optional DHCP quorum identity, durable log and snapshots
dhcp.lock       # exclusive service directory ownership
local.db        # (kube driver only) last-good snapshot; (all drivers) response-cache dump on shutdown
audit-spool.db  # (kube driver only) hash-chained audit events of this node (D156)
lists/          # sha256(url) disk cache of blocklist bodies
keys/           # cookie/encryption key if not provided via env; TSIG/DNSSEC material (encrypted, v0.2+)
edge/           # rendered dnsdist.yml / keepalived.conf (compose profiles mount read-only; supervisors render their own)
backups/        # `kudurru backup` output when --out is omitted
```

Backup: `kudurru backup --out kudurru-backup.tar.zst` = `Store.Snapshot` (`kind: List` YAML) + optional query log; `--safe` strips secrets, sessions, tokens; `kudurru restore` applies it; `kudurru export --safe -o cluster.yaml` emits applyable YAML; the same YAML seeds followers (v0.2) and first boot (`KUDURRU_IMPORT=/path.yaml`). `kudurru import bind` imports zone files. `import pihole|technitium --source-url URL` reads live APIs into an object/settings preview; `--apply` starts a tracked background import through the normal writers (`--wait` waits for its report). The dashboard Tools wizard uses the same converter through `/imports/preview` and `/imports/start`, with per-item selection and durable progress (D169, D184). File importers for the other products remain deferred.

### 6.4 Compose: single instance (+ `edge` profile)

```yaml
# deploy/compose/single/docker-compose.yml
services:
  kudurru:
    image: ghcr.io/robertherbaugh/kudurru:0.1.0
    restart: unless-stopped
    ports: ["53:53/udp", "53:53/tcp", "8080:8080"]            # dropped when profile edge owns :53
    environment:
      KUDURRU_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
      KUDURRU_ENCRYPTION_KEY_FILE: /run/secrets/encryption_key
      TZ: Europe/Berlin
    volumes: ["./kudurru.yaml:/etc/kudurru/kudurru.yaml:ro", "./objects.d:/etc/kudurru/objects.d:ro", "kudurru-data:/var/lib/kudurru"]
    secrets: [admin_password, encryption_key]
    healthcheck: { test: ["CMD", "kudurru", "health", "--ready"], interval: 10s, timeout: 3s, retries: 5, start_period: 60s }
  dnsdist:                                                    # profile: edge (optional)
    profiles: [edge]
    image: ghcr.io/robertherbaugh/kudurru-dnsdist:2.1.2-k0.1.0@sha256:<digest>
    command: ["kudurru", "edge", "run", "--backends", "127.0.0.1:5353", "--tls-dir", "/certs"]
    ports: ["53:53/udp", "53:53/tcp", "853:853/tcp", "853:853/udp", "443:443/tcp", "443:443/udp"]
    volumes: ["certs:/certs:ro"]
    depends_on: [kudurru]
  # with profile edge: kudurru uses network_mode: "service:dnsdist", listeners.dns address 127.0.0.1:5353, proxyProtocolFrom ["127.0.0.1/32"]
volumes: { kudurru-data: {}, certs: {} }
secrets:
  admin_password: { file: ./secrets/admin_password }
  encryption_key: { file: ./secrets/encryption_key }
```

First boot prints the generated admin password once (if no secret), creates the root zone `.` as a forwarder to `upstreams.groups.default`, `ClientGroup/default` + `Policy/default`, and loads the build-time-embedded starter blocklist from the embedded FS so blocking works offline (Gravity's loopback-list idea without the loopback server) [gravity §17.11]. The embedded list is **built at release time from permissively licensed sources only** (candidate: StevenBlack/hosts, MIT — list licences are not covered by the research and must be confirmed in the release checklist) and carries its own NOTICE entry; copyleft-licensed lists (e.g. hagezi's GPL-3.0 lists, AdGuard's filters) may be referenced by URL at runtime like any other data but are never redistributed inside the image, and `licenses.yml` checks the embedded list's provenance file.

### 6.5 Compose HA with dnsdist + keepalived

One directory per node (`deploy/compose/ha/node-{a,b,c}/`), identical except `NODE_NAME`, `NODE_IP`, priority index. **v0.1 `ha-static`:** every node runs the same file-managed configuration (`kudurru.yaml` + `objects.d/` distributed by git/ansible/rsync — `cluster.role: static` makes the API/UI read-only for every `kudurru.io` object, shows "managed by file", and a diagnosis message explains that writes must go through the distributed files); **v0.2:** the same compose with `KUDURRU_CLUSTER_ROLE=follower` on nodes b/c and `KUDURRU_CLUSTER_PRIMARY=https://10.0.0.11:8082` makes the UI writable on the primary and replicated; **v1.0:** `KUDURRU_CLUSTER_PEERS` + `bootstrapExpect` turns the same three nodes into raft voters. The dnsdist and keepalived services do not change across the three versions.

```yaml
# deploy/compose/ha/node-a/docker-compose.yml   (node-a: 10.0.0.11, prio 200; node-b: 10.0.0.12, prio 190; node-c: 10.0.0.13, prio 180)
services:
  kudurru:
    image: ghcr.io/robertherbaugh/kudurru:0.1.0
    network_mode: host                                   # binds :5353 for dnsdist, :8080 API, :8082 internal; the VIP is held by keepalived
    restart: unless-stopped
    environment:
      KUDURRU_NODE__NAME: node-a
      KUDURRU_LISTENERS__DNS: "0.0.0.0:5353"
      KUDURRU_LISTENERS__PROXYPROTOCOLFROM: "127.0.0.1/32,10.0.0.11/32,10.0.0.12/32,10.0.0.13/32"   # exact peers, never the LAN /24
      KUDURRU_LISTENERS__PROXYPROTOCOLSECRETFILE: /run/secrets/proxy_secret
      KUDURRU_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
      KUDURRU_ENCRYPTION_KEY_FILE: /run/secrets/encryption_key
      KUDURRU_LISTENERS__INTERNAL__TOKENFILE: /run/secrets/internal_token         # optional: defaults to a value derived from the encryption key
      # v0.2: KUDURRU_CLUSTER__ROLE=follower  KUDURRU_CLUSTER__PRIMARY=https://10.0.0.11:8082   (nodes b, c)
      # v1.0: KUDURRU_CLUSTER__BOOTSTRAPEXPECT=3  KUDURRU_CLUSTER__PEERS=10.0.0.11:7000,10.0.0.12:7000,10.0.0.13:7000
    volumes: ["./kudurru.yaml:/etc/kudurru/kudurru.yaml:ro", "./objects.d:/etc/kudurru/objects.d:ro", "kudurru-data:/var/lib/kudurru", "kudurru-run:/run/kudurru"]
    secrets: [admin_password, encryption_key, internal_token, proxy_secret]
    healthcheck: { test: ["CMD", "kudurru", "health", "--ready"], interval: 10s, timeout: 3s, retries: 5, start_period: 60s }
  dnsdist:
    image: ghcr.io/robertherbaugh/kudurru-dnsdist:2.1.2-k0.1.0@sha256:<digest>
    network_mode: host                                   # recommended so dnsdist binds the VIP as it appears (0.0.0.0:53)
    restart: unless-stopped
    command: ["kudurru", "edge", "run",
              "--backends", "10.0.0.11:5353,10.0.0.12:5353,10.0.0.13:5353",   # EVERY node lists ALL kudurru nodes
              "--pool-resolver", "chashed", "--pool-authoritative", "leastOutstanding",
              "--tls-dir", "/certs", "--api", "127.0.0.1:8083", "--console", "127.0.0.1:5199",
              "--api-key-out", "/run/kudurru/dnsdist-api.key", "--proxy-protocol-secret-file", "/run/secrets/proxy_secret"]
    secrets: [proxy_secret]
    volumes: ["certs:/certs:ro", "kudurru-run:/run/kudurru"]
    depends_on: [kudurru]
  keepalived:
    image: ghcr.io/robertherbaugh/kudurru-keepalived:2.4.3-k0.1.0@sha256:<digest>
    network_mode: host
    cap_add: [NET_ADMIN, NET_RAW, NET_BROADCAST]
    restart: unless-stopped
    stop_grace_period: 20s                               # keepalived must release the VIP before the container goes
    command: ["kudurru", "vip", "run",
              "--vip", "dns=10.0.0.53/24@eth0#53",        # name=addr/len@iface#vrid  (VRID is hostOnly, never changes on re-render)
              "--vip", "dns2=10.0.0.54/24@eth0#54",       # optional second VIP, reversed priorities → active/active
              "--peers", "10.0.0.12,10.0.0.13", "--priority-index", "0",
              "--hmac-key-file", "/run/secrets/vrrp_hmac",
              "--track-dnsdist-api", "http://127.0.0.1:8083", "--dnsdist-api-key-file", "/run/kudurru/dnsdist-api.key", "--require-backends", "1"]
    volumes: ["kudurru-run:/run/kudurru"]
    secrets: [vrrp_hmac]
    depends_on: [dnsdist]
volumes: { kudurru-data: {}, kudurru-run: {}, certs: {} }
secrets:
  admin_password: { file: ./secrets/admin_password }
  encryption_key: { file: ./secrets/encryption_key }
  internal_token: { file: ./secrets/internal_token }
  proxy_secret:   { file: ./secrets/proxy_secret }
  vrrp_hmac:      { file: ./secrets/vrrp_hmac }
```

Behaviour: keepalived decides VIP ownership only (VRRPv3 unicast, `state BACKUP`, `nopreempt`, `auth_hmac`); `track_script` probes the local dnsdist on :53 udp+tcp and requires ≥ 1 healthy backend; `track_file /run/kudurru/weight-dns` is written by the local kudurru (`0`/`60`/`300`); the FIFO consumer emits `kudurru_vip_master{vip="dns"}`. Because every dnsdist lists all nodes, a node whose kudurru is unhealthy but whose dnsdist is healthy still serves, and query logs stay correct on every node through PROXYv2 [dnsdist §3.2]. External secondaries doing AXFR from `ha-static` twins should transfer from one named node (serials are `max(prev+1, unix_now)` on each twin and therefore close but not identical); follower mode (v0.2) removes that caveat.

---

## 7. Query path and the blocking engine

### 7.1 Step by step (one `A` query from a LAN client, dnsdist in front)

1. **dnsdist** (optional) accepts on :53/:853/:443, ACL, DynBlocks/eBPF drop abuse, packet cache (resolver pool only), picks a backend (`chashed` on qname for cache locality), prepends **PROXYv2** with the real client address, forwards to `kudurru:5353`.
2. **ingress**: parse PROXYv2 only from `proxyProtocolFrom` (else drop), parse the message (FORMERR on garbage), EDNS version check, global ACL, per-prefix QPM limiter (`/32: 600 udp`, `/24: 6000`, IPv6 `/128,/64,/56`; over-limit UDP → TC=1 for a percentage, then drop) [technitium §20.9]; NSID/CHAOS `id.server`/`version.bind`/`hostname.bind` and `health.kudurru.internal.` (TXT `<node-id> <serial-epoch>`, never logged) answered directly.
3. **identify**: client → `ClientGroup`s: exact IP → longest-prefix CIDR → (v0.2) MAC via inventory → hostname/rDNS → DoT/DoH client ID (`<id>.dot.<domain>` SNI, `/dns-query/<id>`) → `default`. Equal-length CIDR ties raise a diagnosis message. The union of matching groups selects `Policy` objects (highest `priority` wins per setting) and the decision travels as `Request.Policy` [pihole §16.5, blocking §2.8].
4. **control**: `Policy`/global rules (`allowedNetworks`, drop malformed, refuse ANY, block qtypes) → `Allow | DropSilently | Refuse`; a drop returns `pipeline.ErrDrop`, after which the transport should send nothing (UDP/DoQ) or close the exchange (v0.1 transports still answer SERVFAIL; dev-docs/followups.md).
5. **postprocess**: wrap local, authoritative and recursive answering stages, then finalize the private client response on unwinding. Clear AD without current local validation provenance or after material synthesis; honor client CD/DO/AD interest. Response synthesis/filtering and TRACEPARENT handling extend this wrapper in v0.2 (D191).
6. **local**: custom host records / synthesized inventory names (UniFi, v0.2), SUDN (`.local`, `.onion`, RFC 6303 reverse without a zone → NXDOMAIN). `Status: local_record`; never cached.
7. **authoritative**: longest-suffix zone match over the in-memory trees (root `.` is a forwarder by default). Enforce `queryAccess`/`queryACL`; answer with CNAME chase (loop cap 20; into other local zones, else into the rest of the pipeline when RD), wildcards by label replacement, NODATA vs NXDOMAIN on name existence (**never NXDOMAIN while unsynced: SERVFAIL** [k8s §2.2]); AXFR/IXFR here (`transferACL`, TSIG, `dns.Transfer.Out`); NOTIFY/UPDATE opcodes routed to handlers (NOTIMP in v0.1). With `spec.fallback` (D185), absent names and ordinary types at an apex with only generated SOA/NS continue through resolver routing when RD and recursion are available; existing names keep missing-type NODATA. ACLs and stale/unsynced snapshot gates still fail closed, and fallback zones refuse transfers and NOTIFY. Local answers bypass blocking; fallback answers continue through it [technitium §3].
8. **blocking** (recursive path only): allow-wins-across-groups → allow-only mode → in-list `@@` exception → deny (`$important` beats `@@`) → after `next` returns, response-side CNAME/A/AAAA/HTTPS-hint inspection against deny sets [blocking §2.7, §6.1]. A hit synthesizes the policy's response (`zeroIP` default, `nxdomain` with SOA, `customIP`, `refused` discouraged), `blockTTL` (10 m default), EDE 15/17, `Status: blocked_*`, `List`/`Rule` for the UI. Temporary disable (`POST /dns/blocking {blocking:false, timer:300}`) is cluster-wide via `Kudurru.status.blocking.disabledUntil`; in v0.1 Kubernetes, where `Kudurru` is still a ConfigMap without a status subresource, the kube driver keeps this one runtime field in the `kudurru-runtime` ConfigMap (annotation `kudurru.io/blocking-disabled-until`, server-side applied by whichever replica served the request, watched by all), and in Docker in `state.db`; `ha-static` twins toggle node-locally (badged).
9. **rewrite**: configured policy rewrites precede forwarding; DNS64, AAAA filtering and NXDOMAIN handling run after raw cache lookup on private responses (D194). Blocking inspects the result, then postprocess applies rebinding protection and final flags. Client-group authoritative views remain later v0.2 work.
10. **cache**: raw-response cache keyed by name, type, class, DO/CD, ECS, captured route/trust scope and publication epoch. Positive/negative TTLs, prefetch, stale serving and coalescing preserve validation provenance and hard proof deadlines. Detached fetches cannot insert after flush or configuration replacement; persisted entries rebind only to matching semantic scopes. Client synthesis and policy filtering happen after raw lookup (D191).
11. **dnssec**: validate raw upstream responses before cache insertion when enabled; bound proof lookups, signature verification and NSEC3 work. Authenticate signed data or unsigned delegations; bogus/indeterminate answers return SERVFAIL with EDE. See [DNSSEC validation](../docs/deploy/dnssec-validation.md) for operator-managed anchors and expiring NTAs. CD bypass and disabled validation use separate cache contexts.
12. **upstream**: `Request.UpstreamGroup` (forward zones, primary-zone fallback, a Policy's group) → else conditional map → group; strategy; dnsproxy exchange with per-group timeout; ECS per `upstreams.ecs.mode` (`strip` default, `forward`, `add` with `ipv4Prefix`/`ipv6Prefix`); `Status: forwarded`, `Upstream: <group/server name>`; errors → `servfail_upstream|timeout_upstream`. Recursive groups resolve from the root using `internal/recursor` within the group timeout, never send ECS, and retain their DNSSEC state.
13. **log**: build the `QueryEvent` (§11.2), non-blocking enqueue to the per-worker ring, atomic counters/histograms, dnstap `CLIENT_RESPONSE` (v0.2), OTel attributes (v0.2). `dnsserver` writes the reply; dnsdist caches it (resolver pool) and answers the client.

D54, D64, D60, D62, D65 and D67 describe the original stages; D191 defines response unwinding and validation/cache provenance.

Hard rules: the query path never touches `Store`, never blocks on logging, never waits on a leader, and keeps answering from the last snapshot when the store is unreachable [gravity §4.4].

### 7.2 Policy model

```
Source     {url|inline|configMapRef, format auto|hosts|domains|adblock|regex, refresh, etag, digest, ruleCount, invalid, enabled}
BlockList  {name, mode block|allow, sources[], response override}            # blocky "group list"
ClientGroup{name, clients[ip|cidr|mac|clientID|hostname|nameGlob]{name,comment}, sources{integrationRef}}
Policy     {name, clientGroupRefs[], blockListRefs[], allowListRefs[], allowOnly, schedule, upstreamGroup, blockMode, blockTTL, queryLog, priority, rewrites[], safeSearch, safeSearchMode, blockedServices[], blockedCategories[], allowedServices[]}
```
Defaults: `ClientGroup/default` (matches everything), `Policy/default`. Pi-hole users map 1:1: Groups → `ClientGroup` + `Policy`, Lists → `BlockList`, Domains (exact/regex allow/deny) → an inline `BlockList` source, Clients → `ClientGroup.clients`. Blocked-services and safe-search data come from a kudurru-authored, Apache-2.0 catalogue embedded in the binary with per-entry provenance (AdGuard's tables are GPL) [blocking §6.6]; `blocking.catalogue.url` can replace it at runtime. It shipped in the v0.1 fix round instead of M2 (D60, D120, contract in docs/deploy/blocked-services.md).

### 7.3 Blocking engine design (performance is a named risk, §14)

Per `BlockList`, compile sources into a **chain of three matchers, cheapest first**; the exact-set layout, the cheapest-first chain and matched-rule reporting are borrowed from blocky (Apache-2.0, NOTICE) [blocking §2.5]:

1. **Exact set**: lower-cased entries bucketed by length, each bucket one sorted concatenated string, binary search, zero allocation.
2. **Suffix set** for `||domain^`, `*.domain` and hosts-style entries with subdomain semantics: TLD-bucketed sorted sets plus a maphash bit filter and a fence index, walking parent labels broadest-first and returning the matched rule. This is kudurru's own design (blocky v0.35 uses a trie, D73); one process-wide maphash seed serves every list's filter. It fixes blocky's reduction of `||domain^` to exact match [blocking §8.2].
3. **Regex scan** indexed by a 4-byte piece of each expression's required literal (1,000 indexed regexes cost ~140 ns per miss); expressions with no such literal are scanned linearly (~25-75 ns each); warning above 50 unindexed regexes, hard cap 1,000 per list (`CompileOptions.RegexHardCap`, D73).
4. (v0.2) **Modifier slow path**: rules with `$important`, `$badfilter`, `$dnstype`, `$dnsrewrite`, `$client`, `$ctag`, `$denyallow`, mid-pattern wildcards: 5-byte shortcut index → candidate verify (urlfilter's idea re-implemented, never imported). Unknown modifier → rule ignored and counted in `status.invalid`, so browser lists are safe. v0.1 parses the whole grammar, compiles pure `||x^`/`@@||x^`/hosts/domains/regex into matchers 1-3 and counts the rest as "unsupported in this version".

Compiled sets live in an `atomic.Pointer[Snapshot]` swapped copy-on-write per refresh; refresh is skipped when the CRC-32 of normalised rule lines is unchanged (AdGuard Home) [blocking §3.3]. The list pipeline is blocky's: ETag/Last-Modified conditional GET, `sha256(url)` disk cache with stale-on-unreachable, tee-to-temp + atomic rename, retry/cooldown, `maxErrorsPerSource`, keep-old-group-on-failure, bounded concurrency, init strategies with disk seeding, HTML/binary detection, `! Title:` extraction, `maxHTTPSize` [blocking §2.6, §6.3].

**Fetch once, serve everywhere (v0.1):** the leader's `listfetcher` downloads, writes `lists/<sha256(url)>`, and publishes `BlockList.status.sources[].digest`; replicas pull `GET :8082/internal/lists/<digest>` from the leader (found via the Lease holder → EndpointSlice), verify the hash and compile locally; fall back to the disk cache, then to independent fetching, after 10 min without a reachable leader [k8s §16.11]. Compilation is node-local (CPU is cheap; a binary snapshot format would be another thing to version).

Match reporting: `Contains(name) → map[list]rule` so the query log explains "blocked by `oisd`: `||ads.example^`", while Prometheus labels carry only list names.

Performance gate in CI (`make bench`): 1 M-entry suffix set, p99 lookup < 2 µs single-threaded, < 150 MB RSS for 3 M entries (steady state after GC in a fresh process; peak during a copy-on-write rebuild is tracked separately and needs roughly old + new + one list in flight), compile < 5 s; a +20 % regression against a committed, runner-class-specific baseline fails the build, each sample in its own process. Measured in the spike (dev-docs/spikes/blocking.md): p99 1.18 µs hit / 0.71 µs miss at 1 M, 90 MB steady RSS (138 MB peak) at 3 M, 0.84 s compile, zero allocations per lookup. List semantics pinned there: `@@` exceptions apply to their own list by default (a selection scope gives the merged behavior); `*.x` also matches `x`; `||x` is read as `||x^`; hosts lines pointing at addresses other than 0.0.0.0/::/127/8/::1 are rewrites and counted unsupported; `localhost` boilerplate is ignored; bare IPs are matched against answer addresses. No published numbers exist for blocky/AGH [blocking §5.1], so kudurru publishes its own; a conformance corpus (OISD, Hagezi, StevenBlack samples) with expected block/allow decisions is cross-checked black-box against an AdGuard Home container (no code copied).

---

## 8. Auth: OIDC flows, RBAC, tokens, bootstrap

### 8.1 Principals and identities

`Principal = User | ServiceAccount | KubernetesServiceAccount | LocalRoot (unix socket) | Node (internal token / mTLS peer)`; Node principals hold implicit `viewer` + `auditor`. The stored identities are internal kinds (`LocalUser`, `APIToken`, `ServiceAccount`, `RoleBinding`, `IdentityProvider`) in `api/internalkinds/v1alpha1` (Go's internal-package rule forbids `api/internal`, D100): Secrets in the release namespace in Kubernetes, JSON rows in `state.db` in Docker, never CRDs. Identity key for OIDC users is `(provider, iss, sub)` — never email; `email` is trusted only when `email_verified` [auth §2.7, §10.4]. A `User` has zero or more identities (local password, OIDC) and local or IdP-managed group memberships.

### 8.2 Web login (BFF, authorization code + PKCE) and sessions

```
GET  /api/v1/auth/oidc/{idp}/start?next=/zones   → pre-auth cookie {state, nonce, verifier, next} (sealed, 10 min) → 302 IdP (S256, nonce)
GET  /api/v1/auth/oidc/{idp}/callback?code&state → exchange (confidential client) → verify id_token (go-oidc, JWKS cache) → nonce
                                                 → claimValidationRules → claimMappings → provisioning policy → bindings
                                                 → Set-Cookie kudurru_session (sealed, HttpOnly, Secure, SameSite=Lax) → 302 next (same-origin only)
GET  /api/v1/auth/me                              → principal, effective bindings, permission matrix, csrf token
POST /api/v1/auth/password                        → {currentPassword,newPassword} (local users); revokes every earlier session, sets a fresh cookie
GET  /api/v1/auth/status                          → {localLogin, localLoginMode, providers, hasDefaultCredentials, session}
POST /api/v1/auth/logout                          → clear cookie; RP-initiated logout via end_session_endpoint when present (v0.2)
```

Session = **sealed cookie** (AES-256-GCM, cluster-shared `KUDURRU_ENCRYPTION_KEY` / `kudurru.io/session-key` Secret) with payload `{sid, principal, roles_snapshot, idp, iat, exp, idle_exp, csrf}` (`roles_snapshot` = the *resolved* bindings, never the raw IdP group list, so a user with hundreds of groups cannot hit the 4 KiB guard and be unable to log in) re-sealed on each request to slide the idle timeout (30 m) under an absolute lifetime (12 h); 4 KiB guard with a hard error; no refresh token in the cookie. This is why any replica can serve any session in a Kubernetes Deployment with no shared DB. Revocation: `User.status.sessionsValidAfter` (set on password change, admin "kill sessions", disable) is checked per request from the store snapshot. Behind a `SessionStore` interface: `sealed` (v0.1 both modes) and `db` (v0.2 Docker; k8s only with an external store, v1.0) which adds session listing/killing and hourly IdP group re-evaluation via an encrypted refresh token — the auth research's preferred model [auth §4.1], accepted as the v0.1 trade-off because the kube driver has no relational store.

Implementation notes from the auth spike (D71): keys are derived per purpose as HKDF-SHA256(`KUDURRU_ENCRYPTION_KEY`, info=`kudurru/<purpose>/v1`) with purposes `session` and `oidc-preauth`, and every envelope carries a 4-byte key id so the `_PREVIOUS` key (D57) opens old cookies; `roles_snapshot` holds grants as `role` or `role@kind:name`, resolved at login and frozen until the next login, so deleting or narrowing a `RoleBinding` must bump `sessionsValidAfter` for its subjects (M3); the session cookie takes the `__Host-` prefix when Secure; the pre-auth cookie is single-use and path-scoped; discovery is lazy and a failing IdP yields `idp_unavailable`; logout clears the browser copy only. A user with 200 IdP groups seals to 546 bytes (raw groups would be 9,210 bytes), 68 zone-scoped grants fit under the 4 KiB guard. Entra-style group-overage claims are refused with an explanation instead of being read as "no groups"; IdP usernames are prefixed `<idp>:` by default so they can never match `User` bindings meant for local accounts; `defaultRole` may never be `admin`. go-oidc verifies neither nonce, `azp` nor `at_hash` and does not fetch distributed claims, so kudurru does the nonce and audience checks itself.

CSRF: Go 1.25 `http.CrossOriginProtection` on mutating routes + `X-Kudurru-CSRF` header required for cookie-authenticated writes + `SameSite=Lax`; bearer requests exempt [auth §4.1]. Security headers: CSP `default-src 'self'`, `X-Frame-Options: DENY`, `nosniff`, HSTS when TLS terminates in kudurru. Multiple providers in `auth.identityProviders[]` / `IdentityProvider` objects; login page lists them; `?local=1` is break-glass.

```go
// internal/auth/session.go
type SessionStore interface {
    Issue(ctx context.Context, s Session) (cookieValue string, err error)      // sealed: encrypt; db: insert row, return random id
    Load(ctx context.Context, cookieValue string) (Session, error)              // verifies seal/row, idle+absolute expiry, sessionsValidAfter
    Touch(ctx context.Context, s *Session) (cookieValue string, changed bool, err error)   // slides idle_exp; sealed re-seals at most once per minute
    Revoke(ctx context.Context, sid string) error                               // db only; sealed returns ErrUnsupported (use sessionsValidAfter)
    List(ctx context.Context, principal string) ([]SessionInfo, error)         // db only
}
```

### 8.3 CLI login

- v0.1: `kudurru login --server https://dns.example --token kdr_…` or `--username/--password` (local admin); credentials in the OS keyring (`zalando/go-keyring`) with a 0600 file fallback; `--context` picks a kubeconfig for CRD verbs.
- v0.2: `kudurru login` opens the browser for loopback + PKCE (ports 8000/18000, public client `kudurru-cli`), `--device` for RFC 8628 (prints user code and client name per RFC 10027); the CLI sends the IdP **ID token** as bearer; the server routes by `iss`, verifies against JWKS, **requires `aud` to contain `cliClientID` (or `clientID`) and, when `aud` has several values, `azp` to equal one of them**, rejects access tokens and tokens minted for other relying parties (otherwise any ID token the IdP issued for an unrelated app could be replayed against kudurru), caches the verdict by `sha256(token)` until min(`exp`, 15 m) (dex issues 24 h ID tokens by default), re-runs claim mapping per call [auth §4.2].

### 8.4 Claims → identity → bindings

Three Kubernetes-shaped layers [auth §4.3]: `claimValidationRules` (`{claim, requiredValue}`; v0.2 CEL `expression`), `claimMappings` (`username{claim,prefix}` with fallback `preferred_username → upn → email → sub`, `groups{claim,prefix}` tolerant of string-or-array and also reading `roles`, `displayName`, `email`), then authorization via bindings on the prefixed remote group (`corp:/dns-admins`) or a `groupMap`. `provisioning: { autoProvision: always|mapped-only|never, syncGroups: bool, defaultRole: "" }` — **strict by default:** a login yielding zero bindings is refused with a clear message (Grafana `role_attribute_strict`); never silently admin (Gravity's mistake) [auth §10.1].

### 8.5 RBAC model and permission matrix

Hand-rolled `(subject, role, scope)` bindings behind an `Authorizer` interface (Casbin/SAR pluggable) [auth §5]:

```go
// internal/rbac/authorizer.go
type Attributes struct{ Resource, Verb string; Scope Scope /* {Kind cluster|zone|zonegroup|clientgroup, Name} */; Namespace, Name string }
type Decision struct{ Allowed bool; Path string /* binding | sar | local | none */; Missing *Attributes; Reason string }
type Authorizer interface {
    Authorize(ctx context.Context, p Principal, a Attributes) (Decision, error)   // pure for the binding authorizer; SAR authorizer caches ≤ 2 min by (token hash, attributes)
}
type Chain []Authorizer   // first Allowed wins (§5.5 precedence); Decision.Path is what the audit row records
```

Every route declares `{resource, verb, scopeFromPath}`; a lint fails the build if a route has no declaration. `internal/rbac/permissions.yaml` is the **single source** that generates the built-in roles, the aggregated ClusterRoles in the chart, the docs table below and the lint fixtures.

Resources × verbs (`read|write|delete` + extras): `zones{transfer,sign,share}`, `records`, `blocklists{refresh}`, `policies`, `clientgroups`, `clients`, `querylog{export,purge}`, `stats`, `cache{flush}`, `blocking{toggle}`, `settings`, `node{config}`, `users`, `groups`, `serviceaccounts`, `tokens`, `identityproviders`, `rolebindings`, `integrations{sync,check,secrets,act}`, `cluster{join,promote,drain}`, `edge`, `audit{export}`.

| Permission | viewer | auditor | operator | zone-admin (scoped) | admin |
|---|---|---|---|---|---|
| zones/records/stats/cache read | ✓ | ✓ | ✓ | ✓ scoped | ✓ |
| records write/delete | | | ✓ (scoped or global) | ✓ scoped | ✓ |
| zones write (options) / delete / transfer / sign / share | | | | ✓ scoped (create needs cluster `zones.write`) | ✓ |
| blocklists/policies/clientgroups/clients read | ✓ | ✓ | ✓ | | ✓ |
| blocklists/policies/clientgroups/clients write, cache flush, blocking toggle | | | ✓ | | ✓ |
| querylog read (with client IPs) | | ✓ | ✓ (`settings.operatorSeesQueryLog`, default true) | | ✓ |
| querylog export / purge | | export | | | ✓ |
| settings read (redacted) | ✓ | ✓ | ✓ | | ✓ |
| settings write, node.config, integrations.*, cluster.*, edge.* | | | | | ✓ |
| users/groups/serviceaccounts/rolebindings/identityproviders | | | | | ✓ |
| own tokens write | ✓ | ✓ | ✓ | ✓ | ✓ |
| audit read | | ✓ | | | ✓ |

Scopes: `cluster`, `zone:<name>`, `zonegroup:<label>` (v0.2), `clientgroup:<name>` (v1.0, so a group owner sees only their clients' queries). The creator of a zone receives `zone-admin` on it (Technitium's initial grant) [auth §9.1]. Evaluation is a pure function with table tests; zone listing filters by scope. `403` responses carry `missing:{resource,verb,scope}` so the UI can explain. Kubernetes mapping: `viewer ≡ kudurru-viewer`, `operator ≡ kudurru-editor`, `admin ≡ kudurru-admin`, `auditor ≡ kudurru-auditor`; `zone-admin` ≡ a namespaced RoleBinding plus `resourceNames` on `dnszones`.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: RoleBinding
metadata: { name: platform-zone-admins, namespace: dns }
spec:
  role: zone-admin
  scope: { kind: zone, name: home-example-com }
  subjects:
  - { kind: Group, name: "corp:/dns-admins" }
  - { kind: ServiceAccount, name: external-dns, namespace: kube-system }   # k8s SA via projected token
```

### 8.6 API tokens and service accounts

Grafana-style service accounts owning N tokens; wire format `kdr_<12-char id>_<43-char secret>`; store `sha256(secret)` only; constant-time compare on the full hash, never prefix lookups (Technitium v15.6 oracle); unknown id and wrong secret return the same 401; optional expiry (`auth.tokens.maxLifetime`), `lastUsedAt/IP`, revocation, audit [auth §4.5]. Personal tokens inherit the owner's bindings ∩ optional scopes and die with the user. Integration credentials (Cloudflare token, UniFi key, dnsdist API key) are outbound secrets, not principals: `secretRef`/file/env only; fingerprints in status.

### 8.7 Local accounts, bootstrap, recovery

- First start with no users: `admin` from `KUDURRU_ADMIN_PASSWORD[_FILE]` else generated and printed once; `mustChangePassword=true`; `hasDefaultCredentials` in `GET /auth/status` so the UI nags [auth §4.4].
- `KUDURRU_BOOTSTRAP_TOKEN[_FILE]` creates a `bootstrap` service-account token (Helm post-install jobs, Terraform) — hash only at rest.
- Argon2id (m=19 MiB, t=2, p=1, PHC string), 1 KiB password cap, global hashing semaphore; per-IP (5 failures → 5 min) and per-user (10 → 15 min) limiters with `429 Retry-After`, dummy hash for unknown users, sanitised usernames in logs [auth §7].
- `localLogin: enabled|admins-only|disabled`; `/login?local=1` break-glass; `kudurru admin reset-password|create-token|list-users` over `auth.adminSocket.path` (default `/run/kudurru/kudurru.sock`) (SO_PEERCRED uid 0, **or the uid the server itself runs as** — 65532 in the distroless image, which is what `docker exec`/`kubectl exec` get without `--user` — or a configured gid) for `docker exec`/`kubectl exec`.
- TOTP (v0.2) with one attempt per second and last-counter replay protection.

### 8.8 Middleware order

`Recover → RequestID → RealIP(trustedProxies) → SecurityHeaders → AccessLog → CrossOriginProtection → Authenticate(bearer kdr_ | bearer JWT by iss | k8s SA token | cookie+CSRF | unix peer | internal token) → LoadPrincipal(disabled? mustChange? sessionsValidAfter?) → Authorize(resource, verb, scope; SAR for k8s identities) → Handler → Audit(outcome, before/after, authz path)`.

Login throttling is enforced by the users-store lockouts in §8.7, before password verification. `api.Options.RateLimit` is an optional hook for `/api/v1/auth/login`; `kudurru serve` leaves it unset and adds no request-rate limiter. A cookie CSRF failure is recorded during authentication and returns 403 `csrf` on protected routes; public routes such as login continue anonymously.

---

## 9. Integration model

### 9.1 Tiers

| Tier | Who | Where | Contract | Version |
|---|---|---|---|---|
| **A. In-tree Go integrations** (`externaldns`, `webhook`; v0.2 `cloudflare`, `unifi`; later `libdns`, `tailscale`, `proxmox`, `mqtt`) | kudurru repo | in-process, **leader-scoped per instance** (`Lease integration-<ns>-<name>`), never on the query path | Go interfaces below, registered at `init()` (Caddy lifecycle: `New → strict unmarshal → Provision → Validate → Start → Stop`) | framework v0.1 |
| **B. Out-of-process HTTP integrations** | third parties | sidecar / Deployment / anywhere | the external-dns webhook routes verbatim + kudurru extensions (`/clients`, `/networks`, `/events`, `/actions`) negotiated by `application/kudurru.integration+json;version=1`; 5xx-only retries, 4xx permanent, 32 MiB cap, bearer or mTLS | server side v0.1 (kudurru as an external-dns target); client side + `kudurru integrations webhook conformance` v0.2 |
| **C. go-plugin subprocesses for query-path hooks** | third parties | subprocess, gRPC, AutoMTLS | `.proto` of the pipeline stage hooks | not before 1.x, only on demand [integrations §5.1] |

Tier B wire shape (v0.2 client side; the server side of the four external-dns routes is v0.1, §5.6): `GET /` returns `{filters:[...], capabilities:[records|clients|hooks|actions], manifest:{...}}` when the kudurru media type is accepted; `GET /clients → [Client]`, `GET /networks → [Network]` (the `ClientSource` shapes of §9.2); `POST /events ← Event` with `X-Kudurru-Signature: sha256=<HMAC of body with the integration's secret>` and `X-Kudurru-Delivery` id (2xx acknowledges, 5xx retries with backoff + dead-letter after 24 h, 4xx drops and raises `Degraded`); `GET /actions → [ActionSpec{name, params JSON Schema, rbac verb}]`, `POST /actions/{name} ← params` [integrations §5.5].

No runtime-loaded in-process code, ever (Technitium's privilege-escalation path) [integrations §7.1, technitium §21.4].

### 9.2 Interfaces (`ee/integration`)

```go
type Manifest struct {
    Type, DisplayName, Version string
    Capabilities []Capability      // RecordSink | ClientSource | Hook | Actions
    IOClass      string            // cloud_polling | local_polling | local_push
    ConfigSchema json.RawMessage   // JSON Schema; also becomes the Integration CRD one-of block and the UI form
    SecretKeys   []SecretKey       // which spec fields are secret refs + how to verify them
    Docs         string
}
type Integration interface {
    Manifest() Manifest
    Provision(ctx context.Context, host Host, spec json.RawMessage, secrets SecretResolver) error // strict unmarshal
    Validate(ctx context.Context) error              // offline
    Check(ctx context.Context) (Status, error)       // network: auth + reachability (token verify, /v1/info)
    Start(ctx context.Context) error                 // runs until cancelled; leader-scoped
    Stop(ctx context.Context) error
}
type RecordSink interface {                          // outbound; Endpoint/Changes are external-dns wire shapes
    DomainFilter() DomainFilter
    Records(ctx context.Context, zone string) ([]Endpoint, error)
    AdjustEndpoints(ctx context.Context, eps []Endpoint) ([]Endpoint, error)
    ApplyChanges(ctx context.Context, zone string, ch Changes) (ApplyResult, error)   // returns provider IDs for the ledger
}
type ClientSource interface {                        // inbound inventory
    ListClients(ctx context.Context) ([]Client, error)     // {ID, MACs, IPs, Name, Hostname, Network{Name,VLAN,CIDR,Domain}, FixedIP, Tags, LastSeen, Attributes}
    ListNetworks(ctx context.Context) ([]Network, error)
}
type Hook interface { Topics() []string; Handle(ctx context.Context, ev Event) error }   // at-least-once; host retries + dead-letter
type ActionProvider interface { Actions() []ActionSpec; Invoke(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error) }
type Host interface {                                // what the host lends an integration
    Zones() ZoneReader; Inventory() InventoryWriter; Ledger(name string) Ledger; Events() EventBus
    Logger() *slog.Logger; Metrics() prometheus.Registerer; Status() StatusWriter; Clock() clock.Clock
    Resolve(ctx context.Context, q dns.Question) (*dns.Msg, error)   // re-enter the pipeline (Technitium DirectQuery)
}
func Register(m Manifest, ctor func() Integration)   // panics on duplicate type
```

Host-owned, not per-integration: polling cadence, jitter, backoff, soft/hard error classification (429/5xx/EOF → requeue honouring `Retry-After`), rate budgets, dry-run preview, status conditions `Ready/Authenticated/Synced/Degraded`, secret-rotation re-provisioning (file watch / Secret informer), the **plan engine** (per-`(dnsName,setIdentifier)` table grouped by type, CNAME exclusivity, owner mismatch → skip + `Conflict`, policies `sync|upsert-only|create-only`; ~400 lines re-implemented with a conformance test against external-dns `plan_test.go` cases) and the **ledger** (`(integration, zone, name, type) → {providerIDs[], contentHash, generation, lastApplied}` as an internal kind) [integrations §5.2, §5.4].

Lifecycle events (the hook Technitium lacks): `zone.changed{zone, serial, delta}`, `record.changed`, `client.seen|left`, `query.blocked` (sampled), `blocking.toggled`, `integration.error`, `cluster.leader_changed`, `vip.master_changed`. Debounced 2-5 s; every sink also has a periodic full resync (default 10 m) and a manual "sync now" [integrations §7.9].

### 9.3 Declaring integrations: one schema, two carriers

```yaml
# Docker: kudurru.yaml → objects[] or objects.d/           # Kubernetes: kind Integration (v0.2)
apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: { name: cf-public, namespace: dns }
spec:
  type: cloudflare
  credentials: { apiToken: { secretRef: { name: cloudflare-token, key: token } } }   # Docker: { file: /run/secrets/cf_token }; never inline
  cloudflare:
    zones:
    - zone: example.com
      zoneID: 023e105f4ecef8ad9ca31a8372d0c353          # optional; resolved via Zone:Read
      select: { labels: { kudurru.io/publish: cloudflare }, types: [A, AAAA, CNAME, TXT, MX, SRV, CAA], exclude: ["*.internal.example.com"] }
      defaults: { proxied: false, ttl: 1, comment: "kudurru:owner=home" }
      policy: sync                                      # sync | upsert-only | create-only
      adoptFrom: { externalDNSOwner: k8s-home }         # read heritage=external-dns TXT records and adopt
      adoptUnowned: false
    deletionPolicy: Retain
  resync: 10m
  suspend: false
status:
  conditions:
  - { type: Authenticated, status: "True", reason: TokenVerified }
  - { type: Synced, status: "False", reason: RateLimited, message: "429 retry-after 37s" }
  - { type: Ready, status: "True", reason: Running }
  lastSyncTime: "2026-10-10T21:03:11Z"
  records: { managed: 42, drift: 1, conflicts: 0 }
  provider: { rateLimitRemaining: 1150, sdk: cloudflare-go v7.12.0 }
```
CEL: `has(self.cloudflare) == (self.type == 'cloudflare')`. Spec never contains secret values; status shows fingerprints only. RBAC verbs: `integrations.read|write|secrets|act`.

### 9.4 Worked example 1 — Cloudflare split-horizon (v0.2)

Problem: `example.com` is authoritative inside (RFC 1918 answers) and public at Cloudflare; a subset (public hosts, ACME TXT, MX/SPF) must exist publicly, some proxied, without a second hand-maintained zone [integrations §2.5].

1. **One zone, one truth.** Records opt in with label `kudurru.io/publish: cloudflare` and/or `DNSZone.spec.publish[]` rules (selector + name glob + types).
2. **Per-sink overrides and views.** `providerSpecific: [{cloudflare/targets: "203.0.113.10"}, {cloudflare/proxied: "true"}]` publishes a different value than is served internally; an endpoint with `kudurru.view: public` is *only* published, never served (e.g. the tunnel CNAME in §4.1). Sink-level `translate: {"203.0.113.0/24": "10.0.0.0/24"}` offers Technitium-style 1:1 NAT for bulk cases. Proxied records force `ttl: 1`; only A/AAAA/CNAME are proxiable; proxied public answers are never mirrored inward.
3. **Ownership.** Internal ledger + Cloudflare `comment: kudurru:owner=<clusterID>` (works on Free) + tag `kudurru-owner:<id>` when the plan allows; external-dns `heritage=` TXT records are read for `adoptFrom` migrations, never written. Unowned records are never modified unless `adoptUnowned: true`; foreign-owned → `Conflict` + skip.
4. **Reconcile (leader, level-triggered).** `desired = select(zone) ▷ overrides/views ▷ AdjustEndpoints`; `current = GET /zones/{id}/dns_records?per_page=5000[&comment.startswith=kudurru:]` cached per resync; plan; apply via `POST /zones/{id}/dns_records/batch` in 200-op chunks (deletes → patches → puts → posts), 1 s between chunks, per-record fallback on chunk failure; a shared token bucket budgeted at 1,000 of the **1,200 requests / 5 min per user** limit (cumulative across dashboard, keys and tokens of that user; exceeding it blocks *all* calls for 5 min [integrations §2.1]) across every Cloudflare sink whose token belongs to the same user; 429/5xx → requeue with `Retry-After`; status `Synced`, `records{managed,drift,conflicts}`, `rateLimitRemaining`.
5. **Token.** `GET /user/tokens/verify` + `GET /zones?name=` at `Check()`; documented scope `Zone:Read + DNS:Edit` restricted to the zone; `cloudflare-go/v7` pinned exactly and wrapped thinly.
6. **Status.** `DNSZone.status.published[]` and `DNSRecordSet.status.publications[]{integration, state Accepted|Programmed|Failed}` (external-dns `DNSRecord` idiom) so `kubectl get rrset` shows whether Cloudflare is in sync; `kudurru integrations preview cf-public` prints the dry-run plan.

### 9.5 Worked example 2 — UniFi Network (v0.2)

Inbound `ClientSource` (polling, official Integration API `X-API-Key` at `/proxy/network/integration/v1`: sites, clients, networks, DNS policies with `offset/limit` paging) via `unpoller/unifi/v6` (**read-only** library [integrations §3.4]); legacy `/rest/user`, `/stat/sta`, `/v2/api/site/<site>/active-leases` **only** for alias/hostname/fixed-IP/VLAN/leases, behind version detection (`GET /v1/info`) and per-endpoint 404 tolerance [integrations §3]. The optional push paths below need writes that unpoller does not provide: a thin in-house client (≈ 200 lines, `X-API-Key`) over the official `PUT /v1/sites/{site}/networks/{id}` and `POST|PUT /v1/sites/{site}/dns/policies` endpoints, and the legacy `cmd/stamgr` for block/unblock behind the same legacy flag; `filipowm/go-unifi` (MPL-2.0) is acceptable as an unmodified dependency if the in-house client grows.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: { name: unifi-home, namespace: dns }
spec:
  type: unifi
  credentials: { apiKey: { secretRef: { name: unifi-key, key: apiKey } } }      # Docker: { file: /run/secrets/unifi_key }
  unifi:
    endpoint: https://192.168.1.1
    tlsFingerprint: "sha256:…"                        # instead of insecureSkipVerify
    site: default
    poll: 60s
    inventory: { zoneRef: { name: lan-example-com }, reverseZones: auto, nameTemplate: "{{ .Alias | default .Hostname | slug }}", ttl: 120, includeNetworks: [LAN, IoT, Kids] }
    groups: { byNetwork: { IoT: iot, Kids: kids, Guest: guest }, byNote: "kudurru:group=" }
    push: { dhcpDNSServers: false, mirrorRecords: { enabled: false, types: [A, CNAME] } }
```

What kudurru does with it: networks → `Network` inventory → auto-created reverse zones and `ClientGroup` membership by VLAN; clients → synthesised A/AAAA + PTR `DNSRecordSet`s (`managedBy: integration:unifi-home`, read-only "synthesised" badge, deterministic slug dedupe `-2`, stable per MAC, TTL 120, expire after 3 missed polls); `identify` (§7.1 step 3) matches by MAC through the same inventory; `client.seen/left` events for Hooks. Optional push (`integrations.act`): write the VIP into DHCP DNS servers, mirror selected records into UniFi DNS policies so the gateway answers if kudurru is down, `block-client`/`unblock-client` dashboard buttons via `stamgr`.

### 9.6 external-dns (v0.1, inbound)

`kudurru serve --externaldns-webhook :8888` (or the `externalDNS.webhook` chart values) serves the four routes over the zone store; `external-dns --provider=webhook --webhook-provider-url=http://localhost:8888` makes Ingress/Service/Gateway hostnames flow into kudurru zones with zero kudurru-side Kubernetes code. In v0.1 because it is a thin HTTP façade over the record model and proves the `Endpoint` superset early.

---

## 10. Dashboard / UI plan and API surface

"Pi-hole-grade" means the twelve principles extracted in [pihole §2], re-derived (EUPL code/markup never copied), plus multi-node from day one:

1. Four stat tiles — **Total queries · Blocked · % blocked · Domains on lists** (+ active/total clients in the footer) — each a link into the pre-filtered query log; one `GET /stats/summary` feeds tiles and the sidebar (QPS, status dot, blocking countdown, per-node health dots).
2. 24 h stacked bars (`total/blocked/cached/forwarded/authoritative/other` from `rollup_1m` at `step=auto`), per-client stacked chart with "other clients"; click a bucket → log ±5 min. uPlot for the live series, ECharts for pies/top lists.
3. Everything drills down: top domains/clients/upstreams/lists rows → `queries?domain=…&status_class=blocked`, etc.
4. One colour code via semantic CSS classes `q-permitted / q-cached / q-blocked / q-error`; charts read colours from CSS; themes auto/light/dark/high-contrast(+dark)/one fun theme, delivered as Astryx theme packages (D69).
5. Query log: keyset pagination (`cursor` on `id`, stable under live inserts), `records_total/filtered`, `earliest_ts_memory|disk`, `/queries/suggestions`, wildcard search, `source=auto|memory|disk`, **live tail over SSE**, per-row **Allow/Deny** (`POST /queries/{id}/actions`) with toast, expandable "why" row (status, matched list + rule, CNAME chain, upstream, cache/stale, DNSSEC, EDE, reply type/time, **node**, trace link).
6. Lists / Groups / Policies / Clients pages with `Default` group auto-assignment, `comment` and `enabled` on everything, multi-add arrays returning `processed{success,errors}`, `:batchDelete`, list health (OK/unchanged/unavailable-cached/invalid, counts, last update), "managed by file/Kubernetes/integration" badges with the owner.
7. Disable blocking with countdown (10 s / 30 s / 5 m / custom / indefinite) — cluster-wide.
8. Diagnosis messages badge (regex errors, CIDR tie, list unreachable, cert expiry, disk/WAL size, serial divergence between nodes, dnsdist backend down, serving-from-snapshot, fenced/follower read-only, 2-node quorum warning).
9. Node selector + "all nodes" on every page; Nodes page (version, roles, ready, qps, leader, VIP master, snapshot age) from scatter-gather.
10. Settings: Basic/Expert toggle; "All settings" page generated from the config metadata with `data-configkey` binding; `hostOnly`/env-locked keys shown read-only with the reason.
11. Zones: list (type, serial, records, Ready, managed-by), typed per-RR record editor (A/AAAA/CNAME/MX/SRV/TXT/CAA/HTTPS/SVCB/NS/PTR/TLSA/SSHFP/NAPTR/raw), BIND import/export, transfer/notify status, "publish to" column and DNSSEC panel (v0.2/v1.0).
12. Long-term views (the most-requested missing Pi-hole v6 feature [pihole §17.3]): date-range history and top lists from rollups with `approximate: true` badges when top-N merging is lossy — **in v0.1**.
13. Cluster page (nodes, leader, edge/dnsdist backends from `/edge`, VIP owners, join wizard emitting compose/Helm snippets in v0.2), Integrations page (manifest-driven form, conditions, preview plan, inventory view), IAM, Audit, Tools (dig, import, backup/export, doctor).

Stack (D69): Bun toolchain, React 19.2 + TypeScript, Astryx components and theming (prebuilt CSS, themes as CSS custom properties, `swizzle` for deep customisation), TanStack Query (+ Table/Virtual for the query log), uPlot for live series and ECharts for pies/top lists until `@astryxdesign/charts` is stable; generated TypeScript client from the huma OpenAPI; SSE via `EventSource`; strict CSP; no CDN; `go:embed`ded; path-prefix aware. Breaking API changes are versioned (`/api/v1`) and deprecated routes survive one major [pihole §17.6].

API surface (`/api/v1`; every body carries `took_ms`; errors `{error:{key,message,hint}}`; 201+Location; 204 on delete; `If-Match` on `resourceVersion`; `scope=node|cluster&node=<name>` on stats/log calls):

```
auth:      GET /auth/status · POST /auth/login · GET /auth/oidc/{idp}/start|callback · POST /auth/logout · GET /auth/me · POST /auth/password
           POST|GET|DELETE /auth/tokens[/{id}] · GET|DELETE /auth/sessions[/{id}] (v0.2, db store) · POST /auth/backchannel-logout (v1.0)
objects:   GET|PUT|PATCH|DELETE /objects/{kind}[/{ns}]/{name} · GET /objects/{kind} · POST /apply (multi-doc YAML, whole-object apply)   # what `kudurru get|apply` uses
zones:     GET|POST /zones · GET|PUT|PATCH|DELETE /zones/{zone} · GET|POST /zones/{zone}/records · PUT|DELETE /zones/{zone}/records/{name}/{type}
           POST /zones/{zone}/import (bind) · GET /zones/{zone}/export · POST /zones/{zone}/notify · POST /zones/{zone}/sign (v1.0) · GET /zones/{zone}/bindings
lists:     CRUD /blocklists · POST /blocklists/{name}/refresh · GET /blocklists/{name}/search?domain=
policy:    CRUD /clientgroups · CRUD /policies · GET /clients (inventory) · GET /clients/_suggestions
           POST /domains/{allow|deny}/{exact|regex} (arrays; processed{success,errors}) · POST /{kind}:batchDelete
blocking:  GET|POST /dns/blocking {blocking,timer,groups?} · POST /cache/flush · POST /dns/query (built-in dig)
stats:     GET /stats/summary · /stats/timeseries · /stats/top?dim= · /stats/clients/timeseries · /stats/upstreams · /stats/recent_blocked · GET /padd
log:       GET /queries · /queries/suggestions · /queries/export · SSE /queries/stream · POST /queries/{id}/actions · DELETE /querylog · GET /querylog/status
nodes:     GET /nodes · GET /nodes/{name}/health · GET /cluster · POST /cluster/promote (v0.2) · POST /cluster/join-token (v1.0) · GET /edge (dnsdist backends, VIP owners)
settings:  GET|PATCH /settings (cluster) · GET|PATCH /node/config (hostOnly keys rejected) · GET /settings/_schema
iam:       CRUD /iam/users|groups|serviceaccounts|rolebindings|identityproviders   (v0.1 via YAML; API v0.2)
integr.:   CRUD /integrations · POST /integrations/{name}/sync|check|preview · POST /integrations/{name}/actions/{a}   (v0.2)
audit:     GET /audit · GET /audit/export
misc:      GET /endpoints · GET /openapi.json · GET /docs · GET /info/messages (diagnosis) · GET /version
external:  /externaldns/* (external-dns webhook contract, :8888)
internal:  :8082 /internal/status · /internal/zones (serial map) · /internal/lists/{digest} · /internal/events (SSE) · /internal/snapshot (follower pull, v0.2)   # node/internal-token auth only
compat:    optional Pi-hole v6 façade under /api/pihole/* (v1.0 module)
```

---

## 11. Observability

### 11.1 Metrics (Prometheus, `:9153/metrics`, OpenMetrics with exemplars when tracing is on)

Naming: single prefix `kudurru_`, base units, `_total`, `_info`; `node` comes from the scrape, never from kudurru. **Bounded labels only**: `proto`, `family`, `qtype` (fixed list + `other`), `rcode`, `status` (≤ 32), `status_class`, `zone` (configured zones, else `_other`), `upstream`/`list`/`group` (configured names, ≤ 64 else `_other`), `sink`, `role`, `vip`, `integration`. `client` and `domain` are **never** labels (`metrics.clientLabels: topN` opt-in only). Budget < 10 k series per node [observability §3.2].

```
kudurru_build_info{version,revision,goversion}   kudurru_up_since_seconds   kudurru_config_reloads_total{result}
kudurru_role_active{role}   kudurru_leader{role}   kudurru_blocking_enabled{group}
kudurru_dns_requests_total{proto,family,qtype,zone}   kudurru_dns_responses_total{proto,rcode,status,zone}
kudurru_dns_request_duration_seconds{proto,status_class}   (buckets .0005 … 1 + native)   kudurru_dns_request_size_bytes{proto}   kudurru_dns_response_size_bytes{proto}
kudurru_dns_inflight_requests{proto}   kudurru_dns_dropped_total{reason}  (proxy_unauthenticated, proxy_untrusted_peer, proxy_malformed, ratelimit)   kudurru_ratelimit_buckets   kudurru_dns_do_requests_total   kudurru_dns_edns_total{feature}
kudurru_dns_tcp_connections{proto}   kudurru_dns_tls_handshakes_total{proto,result}   kudurru_dns_doh_http_responses_total{status}
kudurru_block_decisions_total{action,source,group}   kudurru_block_list_hits_total{list}
kudurru_blocklist_entries{list}   kudurru_blocklist_last_update_timestamp_seconds{list}   kudurru_blocklist_update_failures_total{list}   kudurru_blocklist_stale{list}
kudurru_cache_entries{kind}   kudurru_cache_requests_total   kudurru_cache_hits_total{kind,stale}   kudurru_cache_evictions_total{reason}   kudurru_cache_prefetches_total
kudurru_upstream_requests_total{upstream,proto}   kudurru_upstream_responses_total{upstream,rcode}   kudurru_upstream_request_duration_seconds{upstream}
kudurru_upstream_errors_total{upstream,error}   kudurru_upstream_healthy{upstream}   kudurru_upstream_inflight{upstream}
kudurru_recursor_outgoing_queries_total{result}   kudurru_recursor_validations_total{result}   kudurru_recursor_cache_entries   (recursive groups, D170; the upstream metrics name them upstream="<group>/recursive")
kudurru_zone_records{zone,kind}   kudurru_zone_serial{zone}   kudurru_zone_transfers_total{zone,direction,result}   kudurru_dnssec_key_expiry_timestamp_seconds{zone,key}
kudurru_querylog_events_total   kudurru_querylog_dropped_total{stage,sink}   kudurru_querylog_batch_rows   kudurru_querylog_write_duration_seconds
kudurru_querylog_db_bytes   kudurru_querylog_wal_bytes   kudurru_querylog_rows   kudurru_querylog_oldest_timestamp_seconds
kudurru_sink_sent_total{sink}   kudurru_sink_errors_total{sink}   kudurru_sink_queue_length{sink}   kudurru_sink_last_success_timestamp_seconds{sink}
kudurru_store_operations_total{op,result}   kudurru_store_operation_duration_seconds{op}   kudurru_store_watch_resyncs_total{kind} (zonecache re-lists)
kudurru_store_watch_catchups_total{kind} (driver feed catch-ups)   kudurru_store_watchers
kudurru_store_snapshot_serving   kudurru_store_snapshot_age_seconds   kudurru_store_schema_version
kudurru_cluster_members   kudurru_cluster_leader_changes_total   kudurru_cluster_fenced   kudurru_cluster_epoch   kudurru_raft_{term,commit_index,last_contact_seconds} (v1.0)
kudurru_api_requests_total{method,route,code}   kudurru_api_request_duration_seconds{route}   kudurru_auth_events_total{kind,method}   kudurru_audit_events_total{result}
kudurru_health_check_duration_seconds   kudurru_health_check_failures_total
kudurru_vip_master{vip}   kudurru_vip_weight{vip}   kudurru_edge_backend_up{backend} (supervisor, from the dnsdist API)
kudurru_integration_sync_duration_seconds{integration}   kudurru_integration_changes_total{integration,op}   kudurru_integration_errors_total{integration,class}   kudurru_integration_rate_limit_remaining{integration}
```
dnsdist's own `dnsdist_*` metrics (`webserver` `/metrics`, `prometheus_add_instance`) are scraped alongside.

### 11.2 Query log pipeline and SQLite schema (per node)

```
listener workers ─(per-worker SPSC ring 4096, non-blocking; overflow = drop + metric)─▶ collector
  collector: assign id, apply privacy transform ONCE, dictionary ids via LRU, update minute accumulators, append to hot ring (65 536)
  ├─▶ batch writer: 1 000 rows or 1 s → one BEGIN IMMEDIATE…COMMIT (prepared statements); 30 s completion grace for in-progress queries
  ├─▶ rollup flush at minute boundary → rollup_1m/_client_1m/_qtype_1m/_upstream_1m; hourly/daily jobs derive _1h/_1d and domain/list/zone hourly tables
  └─▶ sinks (bounded queues, drop counters, power-of-ten warnings): dnstap (v0.2), clickhouse, loki, webhook, otlp_logs (v0.2+)
```

```sql
-- /var/lib/kudurru/querylog.db   [observability §2.3]
PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA auto_vacuum=INCREMENTAL;
PRAGMA wal_autocheckpoint=2000; PRAGMA mmap_size=268435456; PRAGMA busy_timeout=1000; PRAGMA temp_store=MEMORY;

CREATE TABLE meta (key TEXT PRIMARY KEY, value) WITHOUT ROWID;      -- schema_version (2; older files are upgraded in place on open), node_id, first_ts, privacy_mode_at_write

CREATE TABLE dict_domain   (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, etld1 TEXT, first_seen INTEGER, last_seen INTEGER);
CREATE TABLE dict_client   (id INTEGER PRIMARY KEY, ip TEXT NOT NULL, name TEXT, grp TEXT, first_seen INTEGER, last_seen INTEGER, UNIQUE(ip,name));
CREATE TABLE dict_upstream (id INTEGER PRIMARY KEY, addr TEXT NOT NULL UNIQUE);     -- "tls://1.1.1.1:853", "cache", "zone:example.com"
CREATE TABLE dict_zone     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dict_list     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);     -- blocklist / rule source
CREATE TABLE dict_node     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE dict_policy   (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);     -- "namespace/name" of the deciding policy (schema 2)

CREATE TABLE query (
  id          INTEGER PRIMARY KEY,          -- rowid, monotonic per node (time-ordered); keyset cursor
  ts          INTEGER NOT NULL,             -- unix µs
  latency_us  INTEGER,                      -- NULL if unanswered
  node        INTEGER NOT NULL REFERENCES dict_node,
  proto       INTEGER NOT NULL,             -- 0 udp 1 tcp 2 dot 3 doh 4 doq 5 dnscrypt
  client      INTEGER NOT NULL REFERENCES dict_client,
  domain      INTEGER NOT NULL REFERENCES dict_domain,
  qtype       INTEGER NOT NULL, qclass INTEGER NOT NULL DEFAULT 1,
  status      INTEGER NOT NULL,             -- taxonomy below; >= 16 = not answered normally
  rcode       INTEGER NOT NULL,
  reply_type  INTEGER NOT NULL,             -- ip, cname, nodata, nxdomain, … blob
  upstream    INTEGER REFERENCES dict_upstream,
  zone        INTEGER REFERENCES dict_zone,
  list        INTEGER REFERENCES dict_list,
  rule        TEXT,                         -- matched rule text (bounded 256 B), NULL unless blocked
  cname       INTEGER REFERENCES dict_domain,
  dnssec      INTEGER NOT NULL DEFAULT 0,   -- unknown, secure, insecure, bogus, indeterminate
  ede         INTEGER,
  flags       INTEGER NOT NULL DEFAULT 0,   -- RD, DO, TC, AD, CD, cached, stale, prefetch, ecs, traceparent
  ecs         TEXT,
  trace_id    BLOB,                         -- 16 bytes, NULL unless traced
  answer      TEXT,                         -- short summary (first ~3 RRs), NULL in restricted privacy modes
  policy      INTEGER REFERENCES dict_policy -- deciding policy (schema 2; NULL on older rows and when no policy applied)
);
-- Deliberately four indexes only (each costs ~10-15 B/row and write amplification):
CREATE INDEX query_ts        ON query(ts);
CREATE INDEX query_client_ts ON query(client, ts);
CREATE INDEX query_domain_ts ON query(domain, ts);
CREATE INDEX query_status_ts ON query(status, ts) WHERE status >= 16;   -- partial: blocked/error rows (recent-blocked list)

CREATE TABLE rollup_1m (
  ts INTEGER NOT NULL, node INTEGER NOT NULL,
  total INTEGER NOT NULL, blocked INTEGER NOT NULL, cached INTEGER NOT NULL, forwarded INTEGER NOT NULL, authoritative INTEGER NOT NULL, dropped INTEGER NOT NULL,
  rc_noerror INTEGER, rc_nxdomain INTEGER, rc_servfail INTEGER, rc_refused INTEGER, rc_other INTEGER,
  udp INTEGER, tcp INTEGER, dot INTEGER, doh INTEGER, doq INTEGER,
  lat_sum_us INTEGER, lat_cnt INTEGER,
  lat_b0 INTEGER, lat_b1 INTEGER, lat_b2 INTEGER, lat_b3 INTEGER, lat_b4 INTEGER, lat_b5 INTEGER,
  lat_b6 INTEGER, lat_b7 INTEGER, lat_b8 INTEGER, lat_b9 INTEGER, lat_b10 INTEGER, lat_b11 INTEGER,
  -- bucket upper bounds (ms): 0.5,1,2,5,10,25,50,100,250,500,1000,+Inf  (identical to the Prometheus histogram so both agree)
  PRIMARY KEY (ts, node)) WITHOUT ROWID;
CREATE TABLE rollup_client_1m   (ts INTEGER, node INTEGER, client INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,client)) WITHOUT ROWID;
CREATE TABLE rollup_qtype_1m    (ts INTEGER, node INTEGER, qtype INTEGER, total INTEGER, PRIMARY KEY (ts,node,qtype)) WITHOUT ROWID;
CREATE TABLE rollup_upstream_1m (ts INTEGER, node INTEGER, upstream INTEGER, total INTEGER, errors INTEGER, timeouts INTEGER, lat_sum_us INTEGER, lat_cnt INTEGER,
                                 lat_b0 INTEGER, lat_b1 INTEGER, lat_b2 INTEGER, lat_b3 INTEGER, lat_b4 INTEGER, lat_b5 INTEGER,
                                 lat_b6 INTEGER, lat_b7 INTEGER, lat_b8 INTEGER, lat_b9 INTEGER, lat_b10 INTEGER, lat_b11 INTEGER,
                                 PRIMARY KEY (ts,node,upstream)) WITHOUT ROWID;
CREATE TABLE rollup_domain_1h   (ts INTEGER, node INTEGER, domain INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,domain)) WITHOUT ROWID;
CREATE TABLE rollup_list_1h     (ts INTEGER, node INTEGER, list INTEGER, hits INTEGER, PRIMARY KEY (ts,node,list)) WITHOUT ROWID;
CREATE TABLE rollup_zone_1h     (ts INTEGER, node INTEGER, zone INTEGER, total INTEGER, PRIMARY KEY (ts,node,zone)) WITHOUT ROWID;
-- rollup_1h, rollup_client_1h, rollup_qtype_1h, rollup_upstream_1h, rollup_1d, rollup_client_1d, rollup_domain_1d have identical shapes.
CREATE INDEX rollup_domain_1h_ts ON rollup_domain_1h(ts);
```

Status taxonomy (`status` column; API returns symbolic names; UI colours by class): 0 unknown, 1 forwarded, 2 cached, 3 cached_stale, 4 authoritative, 5 local_record, 6 prefetch_refresh, 7 special_domain, 8 retried, 9 in_progress, 16 blocked_list, 17 blocked_regex, 18 blocked_exact, 19 blocked_cname, 20 blocked_upstream, 21 blocked_rpz (RPZ feed hits, live from v0.2 per D61), 22 blocked_safesearch, 23 blocked_rate_limit, 24 refused_acl, 25 dropped, 26 servfail_upstream, 27 timeout_upstream, 28 dnssec_bogus.

Retention: daily at a random 03:10-03:50 minute **and** whenever `maxDBBytes` is exceeded, deleting in 10 000-row batches with `incremental_vacuum`; dictionaries pruned by `last_seen`; rollups `_1m` 35 d, `_1h` 400 d, `_1d` forever; `wal_checkpoint(TRUNCATE)` every 5 min only when no reader is active; weekly `ANALYZE`; `PRAGMA optimize` on close. Sizing: ~100 B/row → ~0.9 GB/day at 100 qps; defaults raw 30 d capped at 10 GiB; at 1 000 qps keep 2 days raw and point users at the ClickHouse sink [observability §2.4, §2.5].

Privacy at ingest, irreversible: `full | anonymize_client (/24,/48 or daily-HMAC pseudonym) | hide_domains | hide_domains_clients | nothing`; `exclude.{clients,domains,sudn}`; per-policy `queryLog: false`. Raw dnstap export is a separate, audited switch [observability §2.6].

Multi-node: per-node truth; `scope=cluster` = scatter-gather with 2 s per-node timeout, `partial: true` and `nodes[]` on failure; exact sums for additive rollups (UTC-aligned buckets); top-N merged from `n*4` per node and flagged `approximate` with `cutoff_rank`; uniques by union (≤ 10 k) or HLL; cluster cursors encode per-node cursors; live tail multiplexes per-node SSE streams. With a central PostgreSQL database (D158, D160) `scope=cluster` reads it instead (`source: central`, D161): exact sums and top lists over every node, from the node reader's own code. Never Gravity's in-etcd tsdb [observability §5].

```go
// internal/querylog/sink.go — every external sink; the SQLite writer is the one built-in consumer that may apply back-pressure to nothing (it drops + counts)
type Sink interface {
    Name() string
    Open(ctx context.Context, cfg json.RawMessage, m prometheus.Registerer) error
    Write(batch []*QueryEvent) error      // called from a dedicated goroutine with a bounded queue (default 10 000 events); never from the query path
    Flush(ctx context.Context) error
    Close() error
}
// host owns: queue sizing, drop counters (`kudurru_querylog_dropped_total{stage="sink",sink}`), power-of-ten warnings, retry/backoff, privacy transform applied before enqueue, and whether wire buffers are retained (only while a raw sink such as dnstap is registered)
```

### 11.3 dnstap (v0.2)

Native emission of `CLIENT_QUERY/RESPONSE`, `FORWARDER_QUERY/RESPONSE` (`AUTH_*` for local zones) via `golang-dnstap` `SocketWriter`, queue 10 000, `identity=node`, `version=kudurru/<ver>`, `extra="status=… list=… trace=…"`; `dnstap listen` mode ingests frames from dnsdist/Unbound sidecars into the same log; per-type switches and a sample rate; `raw: true` is an audited admin switch independent of the privacy mode. DNS-collector is the documented path to ClickHouse/Loki/Kafka.

### 11.4 Audit (v0.1)

One immutable `AuditEvent` per mutating API/CLI/reconcile/integration/VIP/auth action: `{id ULID, ts, node, actor{kind,id,name,groups,auth}, action "<resource>.<verb>", resource{kind,id,sub}, result success|denied|error, reason, authz sar|binding|local, diff{before,after} (secrets redacted, 4 KiB cap), request{id,ip,ua,method,path,trace_id}, prev_hash, hash}`; stored in the node's SQLite (`state.db` `audit` table in the same transaction as the mutation for the sqlite driver; the audit spool `audit-spool.db` for the kube driver, D156), hash-chained, 400 d retention; **also** emitted as a JSON line on stdout (`"log":"audit"`) so Kubernetes log pipelines capture it; optional Kubernetes `Event` on the owning CR; `GET /audit` with RBAC `audit.read` [observability §4.2]. Denied attempts are audited from the authz layer. In Kubernetes the per-pod spool rows live on `emptyDir`; every node ships them into immutable `AuditLog` batches in the release namespace (D157), which every replica reads, so `GET /audit` gives the same answer on every replica and survives pod replacement; the stdout JSON line (log pipeline) remains the copy to compare against, and the apiserver audit log covers CRD writes.

### 11.5 Health, logging, tracing

Probes as in §5.2; CHAOS `id.server`/`version.bind`/`hostname.bind` + NSID; `health.kudurru.internal.` TXT for dnsdist and keepalived; `kudurru lameduck 5s` pre-stop. Logs: `log/slog` JSON with per-role levels (`KUDURRU_LOG__LEVEL=info,dns=debug,raft=warn`), request-scoped attributes, trace/span ids; the query log is not the app log. Tracing (v0.2): OTel SDK, OTLP exporter, `parentbased_traceidratio` 0.001 + tail-keep on error/slow (> 250 ms); accept EDNS `TRACEPARENT` (option 65500, configurable) **only from configured proxies** (dnsdist), strip before forwarding to public upstreams, re-add only to upstreams flagged `tracePropagation: true`; spans `kudurru.dns.query` → `acl / blocking.match / cache.lookup / zone.lookup / dnssec.validate / upstream.exchange` with OTel DNS semconv attributes; `trace_id` stored in the query row, as a histogram exemplar, and linked from the UI [observability §4.3].

### 11.6 Shipped dashboards and alert rules (v0.1)

`deploy/grafana/kudurru-overview.json` (rows: tiles, traffic, latency heatmap, blocking, cache, upstreams, zones + **serial-divergence panel** (`max by (zone)(kudurru_zone_serial) - min by (zone)(kudurru_zone_serial)`), pipeline, cluster/API, a dnsdist row from `dnsdist_*`, VIP row) and `deploy/prometheus/rules.yaml` as a `PrometheusRule`:

| Alert | Expression (sketch) | For |
|---|---|---|
| `KudurruNoLeader` | `max(kudurru_leader{role="controller"}) == 0` | 2 m |
| `KudurruServfailRatio` | `sum(rate(kudurru_dns_responses_total{rcode="SERVFAIL"}[5m])) / sum(rate(kudurru_dns_responses_total[5m])) > 0.05` | 5 m |
| `KudurruUpstreamUnhealthy` | `kudurru_upstream_healthy == 0` | 2 m |
| `KudurruQuerylogDrops` | `increase(kudurru_querylog_dropped_total[10m]) > 0` | 0 m |
| `KudurruQuerylogNearCap` | `kudurru_querylog_db_bytes / <max_db_bytes> > 0.9` | 30 m |
| `KudurruSerialDivergence` | `max by (zone)(kudurru_zone_serial) != min by (zone)(kudurru_zone_serial)` | 10 m |
| `KudurruServingFromSnapshot` | `kudurru_store_snapshot_serving == 1` | 15 m |
| `KudurruSnapshotStale` | `kudurru_store_snapshot_age_seconds > 43200` | 0 m |
| `KudurruDNSSECKeyExpiring` | `kudurru_dnssec_key_expiry_timestamp_seconds - time() < 7*86400` | 0 m |
| `KudurruLatencyP95High` | `histogram_quantile(0.95, …kudurru_dns_request_duration_seconds…) > 0.25` | 10 m |
| `KudurruEdgeBackendDown` | `kudurru_edge_backend_up == 0` or `dnsdist_server_*{state="down"}` | 2 m |
| `KudurruVIPNoMaster` | `max by (vip)(kudurru_vip_master) == 0` | 1 m |
| `KudurruClusterFenced` | `kudurru_cluster_fenced == 1` | 0 m |
| `KudurruBlocklistStale` | `time() - kudurru_blocklist_last_update_timestamp_seconds > 3*<refreshInterval>` | 0 m |

---

## 12. Repository layout, build, CI, release artifacts

Go module `github.com/robertherbaugh/kudurru` (confirmed by the git remote; ratify in §15 Q1). Single module, single binary, no cgo, kubebuilder-style `api/` so CRD types, store types, REST types and docs come from one set of structs.

```
kudurru/
├── go.mod                            module github.com/robertherbaugh/kudurru  (go 1.27)
├── cmd/kudurru/                      cobra root: serve · login · get/apply/delete/describe · zones · records · lists · blocking · query · health · lameduck
│                                     · backup/restore/export · import · edge {render,run} · vip {render,run,fifo} · cluster {status,promote,join-token}
│                                     · integrations {list,check,sync,preview,webhook conformance} · admin {reset-password,create-token} · config {get,set,schema}
│                                     · doctor · api · version · completion
├── api/v1alpha1/                     Go types + kubebuilder markers for every kind (CRDs AND the sqlite driver's schema); deepcopy, register, CEL rules
├── internal/
│   ├── config/                       koanf loader, JSON Schema, env/_FILE, settings metadata table (schema.go: hostOnly/envLocked/restartRequired/writeOnly), docs generator
│   ├── dnsserver/                    listeners (udp/tcp/dot/doh[/doq/doh3]), proxyproto, EDNS/TC handling, chaos/health names, TLS reload
│   ├── pipeline/                     Stage contract, fixed chain, Request/Response, Status taxonomy
│   ├── stages/{ingress,identify,control,local,authoritative,blocking,rewrite,cache,dnssec,upstream,postprocess,log}/
│   ├── zone/                         RRset model (Endpoint superset), tree, lookup, serial/contentHash, journal, xfr (out; in v0.2), notify, bind import/export, catalog (v1.0)
│   ├── dnssec/                       (v0.2 validator for forwarded answers; v1.0 signer, NSEC/NSEC3 chains, RFC 7583 key state machine, CDS/CDNSKEY)
│   ├── blocking/                     matchers (from blocky, NOTICE), adblock parser (clean-room), list fetcher, compile/snapshot, conformance corpus
│   ├── policy/                       client identification, ClientGroup/Policy index, schedules
│   ├── cache/                        sharded TTL message cache, prefetch, serve-stale, coalescing, persistence
│   ├── upstream/                     dnsproxy upstream groups, strategies, conditional map, bootstrap, ECS
│   ├── recursor/                     recursive groups (v0.1, D170): iteration, QNAME minimization, delegation/key cache, DNSSEC validation, NSEC/NSEC3 proofs
│   ├── store/                        Store interface; validator (apiextensions + CEL); schema versions/migrations; conformance/
│   │   └── {sqlite,kube,follower,raft}/      drivers (follower v0.2, raft v1.0); kube/ includes local.db snapshot persistence
│   ├── controller/                   one reconciler per kind; status; Events; duplicate detection; serial stamping
│   ├── auth/                         authenticators (bearer kdr_, JWT by iss, k8s SA tokens, cookie, unix peer, internal token), sessions (SessionStore: sealed/db), argon2, oidc
│   ├── rbac/                         permissions.yaml (single source), roles, bindings, Authorizer (local, SAR), route-permission lint, generators (ClusterRoles, docs)
│   ├── api/                          huma routes, envelope, SSE, UI embed, openapi, external-dns webhook server; pihole/ compatibility façade module (v1.0)
│   ├── querylog/                     ring, collector, sqlite writer, rollups, retention, sinks/{dnstap,clickhouse,loki,webhook,otlp}
│   ├── audit/                        hash-chained audit store + stdout emitter + Events
│   ├── metrics/  health/             registry, ops listener, probes, self-query loop, lameduck
│   ├── integration/                  registry, manifest, lifecycle, runner (leader-scoped, per-instance Lease), plan engine, ledger, secrets
│   │   └── {externaldns,webhook,cloudflare,unifi,libdns}/   externaldns+webhook v0.1; cloudflare+unifi v0.2
│   ├── edge/                         topology.go; dnsdist/{render_yaml,render_lua,supervisor,api_client}; keepalived/{render,supervisor,fifo,trackfile}; vip provider interface
│   ├── cluster/                      node identity, membership sources (static|endpointslice|follower|raft), internal listener (:8082), scatter-gather, promote/fencing
│   ├── sources/k8s/                  (v0.2) stripped, indexed informers for Ingress / Gateway API / LoadBalancer Services feeding `DNSZone.type: external`
│   ├── acme/                         (v1.0) certmagic wrapper, DNS-01 solver against kudurru's own zones, leader-only runnable
│   ├── importers/                    live Pi-hole v6 and Technitium API converters; other formats deferred (BIND parsing lives in zone/)
│   └── testutil/                     CoreDNS-style test.Case helpers, fake upstreams, testcontainers helpers, envtest harness
├── dashboard/                        React 19 + Astryx SPA built with Bun; src/api = generated client; dist embedded via dashboard/embed.go (//go:embed all:dist)
├── license-server/                   the licensor's license portal (D165): Bun + Postgres server, Astryx UI, image, compose, Cloudflare Worker; not part of the binary
├── deploy/helm/{kudurru,kudurru-crds}/   Helm charts
├── deploy/kustomize/                 kubebuilder kustomize tree: crd/bases, rbac (generated from permissions.yaml), manager, samples/, overlays/{metallb,kube-vip,cloud-lb-mixed,hostnetwork-daemonset,standalone,with-edge}
├── deploy/compose/{single,single-edge,ha/node-a,ha/node-b,ha/node-c}/   deploy/grafana/  deploy/prometheus/  deploy/install.yaml (generated)
├── images/{dnsdist,keepalived}/Dockerfile   upstream GPL binaries (digest-pinned) + kudurru supervisor; GPL text + source offer in README
├── docs/                             published documentation: guide, deploy guides and runbooks, the architecture tour
├── docs-site/                        Hugo site for docs.kudurru.io: per-role pages, config and CRD references (generated), migration guides
├── dev-docs/                         design record (this file), milestone notes, research, spikes, follow-ups
├── website/                          kudurru.io (Bun + TypeScript, static assets on Cloudflare Workers)
├── hack/                             dev compose (dex, BIND9/Knot for XFR interop, kind + MetalLB), bench fixtures, lint rules, 2-VM keepalived e2e
├── test/{e2e,interop,bench,conformance}/
├── .goreleaser.yaml  Makefile  .golangci.yml  go.mod  LICENSE (Kudurru Community License)  COMMERCIAL_LICENSE_AGREEMENT.md  LICENSING.md  NOTICE (blocky, dnsproxy, CoreDNS, external-dns attributions)  CONTRIBUTING.md
└── .github/workflows/                ci.yml (lint, unit, race, fuzz, conformance, e2e kind), edge.yml (edge-check + nightly 2-VM VRRP), dev-image.yml, promotion.yml, release-candidate.yml + release.yml → publish.yml (D139), security.yml, licenses.yml, cve-bump.yml
```

Build & CI (GitHub Actions):
- `make generate`: controller-gen (CRDs, deepcopy, RBAC), huma OpenAPI dump, `openapi-typescript` client, Go client, config JSON Schema, config docs, CRD docs, RBAC docs/ClusterRoles from `permissions.yaml`.
- `make lint`: golangci-lint, `go vet`, route-permission lint, config-metadata/docs drift test, `go-licenses` denying GPL/EUPL modules, CRD/CEL cost check.
- `make test`: unit (`-race`), **store driver conformance suite against `sqlite` and `kube` (envtest)**, pipeline golden tests (CoreDNS `test.Case` style), fuzz on wire parsers / PROXYv2 / adblock parser / zone file parser, RBAC table tests, plan-engine conformance against external-dns cases.
- `make bench`: blocking matcher and cache benchmarks with the +20 % regression gate (results published to GitHub Pages).
- `make edge-check`: render `dnsdist.yml`/`dnsdist.lua`/`keepalived.conf` fixtures for every topology and validate them inside the pinned images with `dnsdist --check-config` / `keepalived -t`; plus the console `-e newServer` smoke test (the week-1 spike, kept as a permanent canary).
- `make e2e`: testcontainers (dex OIDC, BIND9 secondary doing AXFR/NOTIFY from kudurru, Knot catalog consumer in v1.0, AdGuard Home black-box blocking corpus), kind + Helm install (mixed-protocol Service with MetalLB, Restricted PSS admission, apiserver-outage test: kill the control plane, restart a pod, assert answers from `local.db`), nightly 2-VM keepalived failover with timing recorded.
- Release: goreleaser (binaries linux/darwin amd64/arm64, deb/rpm/apk with a systemd unit, Homebrew tap), `kos:` images (distroless static nonroot, multi-arch, `-debug` variant), cosign keyless signatures + SBOM + SLSA provenance, Helm charts to `oci://ghcr.io/robertherbaugh/charts/{kudurru,kudurru-crds}`, `crds.yaml` + `install.yaml`, compose bundles, npm `@kudurru/api` TS client, docs site. Releases are cut by merging branches, not by pushing tags (D139): `feature` → `main` (a `dev-<PR>` image per pull request) → `release-candidate` (`v<VERSION>-rc.<N>`, a GitHub pre-release) → `release` (`v<VERSION>`, `latest`), with VERSION in the repository root.
- Supply chain: dependabot (exact pin for `cloudflare-go`; the Dockerfile base images; a 7-day cooldown), security.yml on every pull request, on main and daily (Semgrep CE static analysis; `govulncheck`; Trivy on the lockfiles, secrets, Dockerfiles and manifests and on the images; daily, the latest release's checkout and published images; the Makefile's `sast`, `vulncheck` and `trivy-*` targets; CodeQL needs GitHub Advanced Security in a private repository), Trivy on each release candidate's published images, which a release requires (publish.yml), a dnsdist/keepalived CVE tracking job that opens a PR bumping the pinned upstream digest and rebuilding the supervisor images.

---

## 13. Roadmap

### 13.1 v0.1.0 — "stone"

The first release is dated 18 October 2026. See the
[changelog](../CHANGELOG.md) for the release scope and the
[architecture tour](../docs/architecture/README.md) for implemented behavior.
The table below retains the original milestone schedule; the reports
record development measurements and checks, rather than a live release
status. Deployment acceptance procedures are in the
[release checklist](release-checklist.md).

DHCPv4 ships in v0.1.0 (D179, D180) on Linux: standalone or with three-node
quorum failover on SQLite, or on the Kubernetes store with `DHCPScope` and
`DHCPLease` objects and one elected replica serving relayed clients. It includes durable leases, reservations, multiple subnets,
extended trusted relays, conflict detection, network/boot options, complete
API/dashboard management, native/dnsmasq lease import and persistent
dedicated-domain A/PTR ownership. It is disabled by default. DHCPv6 is
excluded. See [deployment and acceptance](../docs/deploy/dhcp.md).

Milestones (exit criteria in parentheses):

| Week | Milestone | Exit criteria |
|---|---|---|
| 1-2 | **Spikes** (§14; not all run before the schedule went ahead: (c), (d), (e), (g) and (i) passed; (f) kind + MetalLB is a CI job since the v0.1 fix round; (a), (b) and (h) are open, dev-docs/followups.md) | dnsdist console-under-YAML verdict; PROXYv2 + chashed smoke; kube driver passes conformance on envtest; blocky matcher port with bench baseline; go-oidc BFF against dex with sealed sessions; mixed-protocol Service on kind + MetalLB (+ one cloud if available); apiextensions/CEL binary-size measurement |
| 3-6 | **M1 resolver + store** (done 2026-10-11, dev-docs/milestones/m1/; the carry-overs closed in M2 except multi-socket DoT/DoQ/DoH3, moved to v0.2) | Do53/DoT/DoH/DoQ/DoH3 + PROXYv2 (D67); pipeline with golden tests; cache; upstream groups; `Store` + `sqlite` + `kube` drivers with conformance; `zonecache` + `local.db` snapshot; CRDs `DNSZone/DNSRecordSet/BlockList/ClientGroup/Policy`; Lease controller stamping serials/conditions/Events |
| 7-9 | **M2 authoritative + blocking** (done 2026-10-11, dev-docs/milestones/m2/, **except the D60 catalog**, which M2 did not build; it was delivered in the v0.1 fix round as an embedded catalog, D120, D124-D126. The fixed-runner bench job moved to v0.2) | primary/forward zones, full RR set, wildcards, CNAME chase, AXFR out + TSIG + NOTIFY out, BIND import/export; blocking engine (exact/suffix/regex, allow-wins/allow-only, response-side checks), leader-fetched lists with digest pull, embedded starter list, the kudurru-owned blocked-services / safe-search catalog as a fetched data source (D60), bench gate; QPM rate limiting; `health.kudurru.internal.` |
| 10-12 | **M3 API, auth, dashboard** (done 2026-10-11, dev-docs/milestones/m3/, but the dashboard was only tested against mock handlers until the v0.1 fix round, D123; settings hot reload (D106) and Scalar self-hosting landed in M5; query-log and stats scatter-gather landed in the v0.1 fix round (#9); Clients scope and cluster live tail are included; IAM pages are read-only) | huma API + generated clients; OIDC BFF + local admin + `kdr_` tokens + RBAC matrix + k8s SA tokens/SAR + audit; config metadata table with `hostOnly`; dashboard (tiles, 24 h chart, per-client chart, top lists, query log with SSE + allow/deny + explain + long-term views, lists/groups/policies/clients, zones editor, settings basic, nodes, diagnosis badge) |
| 13-14 | **M4 deploy + edge** (reported in dev-docs/milestones/m3/, D122; done 2026-10-11 except: the kudurru-keepalived image did not build and `make edge-check` had never passed until the v0.1 fix round; kind + MetalLB runs in CI; two-VM VRRP acceptance is deployment-specific; serve wiring of `vip.NewKeepalived` and `vip.ServiceProvider` moved to v0.2) | distroless image; compose `single`, `single-edge`, `ha-static` (3 nodes + dnsdist + keepalived); supervisor images with `edge run` (restart convergence) and `vip run` (full contract); Helm `kudurru` + `kudurru-crds` (Deployment, mixed-protocol Service + split fallback, Restricted PSS, probes, PDB, `edge.enabled`, `vip.provider`, standalone mode); kustomize overlays; Grafana board + PrometheusRule; `kudurru doctor` |
| 15-16 | **M5 hardening** (dev-docs/milestones/m5/: BIND secondary and dex e2e, fuzz corpus, security reviews, kind CI, saved-snapshot apiserver-outage startup and recovery, published docs site; hardware acceptance evidence is recorded separately from these local checks) | e2e (BIND secondary, dex, kind, apiserver-outage, 2-VM VRRP), fuzz corpus, security review of auth + rendering, docs site, `v0.1.0` tag |

Explicitly **not** in v0.1 and why it is safe:

| Deferred | Seam present in v0.1 |
|---|---|
| Secondary/stub zones, AXFR/IXFR **in**, NOTIFY in, RFC 2136 | `ZoneType` enum + `SecondarySpec`/`StubSpec` exist (rejected by CEL "not supported in this version"); `zone.Journal` records deltas per serial; `Transfer()` capability; NOTIFY/UPDATE opcodes routed to a handler returning NOTIMP |
| DNSSEC validation of forwarded answers, and signing | recursive groups validate since D170 (`internal/recursor`, `Response.DNSSEC`, the query log's `dnssec` column); `dnssec` stage slot (pass-through); `DNSSECSpec` reserved; zone tree separates *user* RRsets from *derived* RRsets (empty layer); IXFR-from-leader path exists for signed data; `status.dnssec.nextResign` reserved |
| Adblock modifiers (`$dnstype`, `$dnsrewrite`, `$client`, `$important`, `$badfilter`, `$denyallow`) | the parser already tokenises modifiers into a counted "slow path" bucket; the `rewrite` stage exists (empty) |
| MAC / DoT-SNI / DoH-path client identification | `Client` struct carries `MACs`, `ClientID`; `identify` has ordered matcher slots; inventory API exists for UniFi to fill |
| Cloudflare sink, UniFi source | integration framework, `Endpoint` superset, `Labels`/`ProviderSpecific`/`kudurru.view`, ledger kind, `DNSZone.spec.publish[]` reserved, `Integration` schema designed; `external-dns`/`webhook` integrations exercise the framework |
| Console diff-apply for dnsdist, dynblock push, cert reload without restart | `edge.Supervisor` already renders/validates/execs; `Topology` is shared by the YAML and Lua renderers; restart convergence stays the fallback |
| Follower mode / raft | `Store` driver slots; `cluster` package membership-source abstraction; `/internal/snapshot` route reserved; `meta.cluster_epoch`/`primary_id` columns exist; `ha-static` compose is byte-compatible with the follower compose |
| IAM/Integration/Kudurru CRDs | the YAML config shape **is** the CRD spec; the kube driver maps kinds to CRDs generically, so adding a kind is registering its CRD |
| CEL claim mapping, CLI OIDC login, TOTP, impersonation mode | `ClaimMapper`, `Authenticator` chain, `IdentityProvider` fields reserved; bearer JWT routed by `iss` from day one; `auth.kubernetes.mode` enum exists with only `sar` accepted |
| IAM write API/UI (local users beyond `admin`, service accounts, role bindings, identity providers via API) | v0.1 manages these as file/ConfigMap objects (`rbac.bindings`, `auth.identityProviders`, `users:`) plus `POST /auth/tokens` (own tokens) and `kudurru admin create-token`; the IAM page is read-only with "managed by file" badges; the generic `/objects/{kind}` route, `managedBy` and the internal `LocalUser`/`ServiceAccount` kinds already carry what v0.2 exposes |
| Session listing/killing, hourly IdP re-evaluation | `SessionStore` interface with `sealed`; `db` implementation lands with the SQLite session table |
| dnstap, OTel, ClickHouse/Loki sinks | `querylog.Sink` interface with bounded queues; `QueryEvent` keeps wire buffers only while a raw sink is registered |
| `external` zone type (Ingress/Gateway/LB) | `ExternalSpec` reserved; informer plumbing exists for EndpointSlices |
| Catalog zones, ACME, Pi-hole façade, per-clientgroup query-log scope | v1.0; `scope{kind: clientgroup}` already parses and is rejected by validation |

### 13.2 v0.2.0 — "clay copies" (~12 weeks)

Secondary + stub zones (AXFR/IXFR in, TSIG, NOTIFY in, XFR over TLS; every replica transfers independently), RFC 2136 UPDATE with `key × owner × type` policies, Active Directory DNS replacement (single-domain forest), DNSSEC **validation** of forwarded answers (+ NTA; recursive groups validate since v0.1, D170), aggressive NSEC caching (RFC 8198) and RFC 5011 trust-anchor updates for recursive groups, adblock modifier slow path, rewrites/DNS64/AAAA filter/rebinding protection, MAC/ClientID identification, `external` zone type, Cloudflare `RecordSink` (split-horizon with views), UniFi `ClientSource` (+ optional push/actions), Tier-B webhook client + conformance CLI, dnsdist console diff-apply + cert reload + dynblock push (if the spike passed), compose HA **follower mode** with fenced promote and a cluster join wizard, `Kudurru`/`Integration`/`IdentityProvider`/`RoleBinding`/`ServiceAccount` CRDs, `auth.kubernetes.mode: impersonate` opt-in, CEL claim rules, CLI OIDC login (loopback PKCE + device flow), TOTP, `db` session store for Docker, RP-initiated logout, dnstap emit/listen, OTel tracing incl. dnsdist TRACEPARENT, ClickHouse/Loki sinks, "All settings" page, importers for blocky/CoreDNS Corefile/Technitium, RPZ feed import as a `BlockList` source format (D61), `v1alpha1` → `v1beta1` plan.

Enterprise-only custom branding is also part of v0.2: organization name, light
and dark logos, browser favicon and accent color on the dashboard, sign-in and
block page. Saved values survive license downgrade; effective rendering returns
to the built-in brand. See [custom branding](../docs/deploy/custom-branding.md).

### 13.3 v1.0 — "temple" (~16-20 weeks)

DNSSEC **signing** on the leader (ECDSAP256SHA256 CSK default, Ed25519 opt-in, NSEC or NSEC3 with RFC 9276 params, CoreDNS `sign` timers/jitter, RFC 7583 key state machine, KSK activation gated on observed parent DS with manual override, ZSK `rolloverDays`, CDS/CDNSKEY, BIND-format key import, keys encrypted in the store, `ldns-verify-zone`/`delv` in CI, leader takeover without re-sign); **catalog zones** (RFC 9432) produced and consumed; **RPZ publishing**: a compiled `BlockList` served as a transferable RPZ zone with NOTIFY to subscribing resolvers (D61); **raft** `Store` driver (hashicorp/raft, SQLite FSM, single-voter bootstrap, learner join via single-permission token, snapshot before membership change, never fork on join failure, `MinVersion` migrations) replacing follower mode; native ACME DNS-01 via certmagic (leader); ClickHouse-backed exact cluster top-N; `clientgroup:` RBAC scope; back-channel logout; Pi-hole v6-compatible façade module; `/api/v1` freeze + deprecation policy; CRD `v1beta1`; third-party security review; published dnsperf numbers per profile; OLM bundle after `v1` CRDs.

Out of scope for 1.0 unless the user asks (§15): replacing CoreDNS for `cluster.local`, online DNSSEC signing (black lies) as default, go-plugin Tier C, embedded etcd. The QNAME-minimizing iterative resolver once listed here ships in v0.1 as an opt-in group mode (D170).

---

## 14. Risks & mitigations

Ordered by impact × probability; each row names the spike or gate that retires the risk and the seam that keeps a wrong guess from forcing a rewrite.

| # | Risk | Why it is real | Mitigation / spike | Seam |
|---|---|---|---|---|
| 1 | **Replication/HA**: owning consensus (membership, join, snapshots, mixed versions) is where Gravity accumulated its worst bugs | [gravity §16/§18.3] | v0.1 writes **no** consensus code; conformance suite per driver; follower mode (v0.2) has trivially understood semantics plus fencing; raft only in v1.0 after an rqlite-style single-voter prototype behind `Store` with a 100 k-record load benchmark | `Store`, whole-object `Apply`, serving reads only snapshots, backup = `Snapshot` YAML |
| 2 | **Cold start / apiserver outage** on small clusters | [k8s §8, SRE lens] | `local.db` snapshot with age-gated readiness; **leader election decoupled from process lifetime** (§5.3, D44 — stock controller-runtime would exit every pod when the lease cannot be renewed); e2e kills the control plane for 5 min, asserts answers continue on every pod and that no pod restarted, then restarts a pod and asserts answers from `local.db`; `persistence.local` PVC opt-in; `system-cluster-critical` | `zonecache` snapshot writer, `cluster.Elector` |
| 3 | **dnsdist orchestration**: no YAML reload; console directives under YAML UNVERIFIED; console wire protocol undocumented | [dnsdist §1.11, §7] | week-1 spike; restart convergence is the v0.1 baseline and the permanent fallback; supervisor execs the `dnsdist` binary (no console client to write); Lua renderer behind the same `Topology`; `make edge-check`; digest pinning + CVE bump job | `edge.Renderer{YAML,Lua}`, `edge.Supervisor` |
| 4 | **keepalived in containers**: host networking + caps, VIP left behind, VRRP misconfig | [dnsdist §2.4/§6] | generator enforces `state BACKUP`, distinct priorities, `nopreempt`, `auth_hmac`, persisted VRID; keepalived 2.4.3 compiled from source (Alpine ships 2.3.4 without `auth_hmac`); explicit `kudurru vip drain` = FAULT (`300`) since `nopreempt` ignores priority drops, while lameduck never touches the VIP (D51, §3.6); `keepalived -t` before swap; `stop_grace_period`; nightly 2-VM e2e recording failover timing for node loss, FAULT drain and `60` no-op; never default in k8s | `vip` provider interface (`Owner()`, `SetWeight()`, events) shared by kube-vip/MetalLB/keepalived |
| 5 | **Sealed-cookie sessions** diverge from the server-side recommendation | [auth §4.1] | small payload, 4 KiB guard, `sessionsValidAfter`, idle + absolute lifetimes, auditor review of the sealing format; `db` store in v0.2 | `SessionStore` |
| 6 | **DNSSEC signing** correctness (NSEC3 chains, ENTs, wildcards, rollover timing) | [go-dns-stack §3] | leader-only signing, continuity on takeover, precomputed chains, `ldns-verify-zone`/`dnssec-verify`/`delv` + BIND validating resolver in e2e; staged to v1.0 after transfers are proven in v0.2 | derived-RRset layer, serial/journal, `DNSSECSpec` |
| 7 | **Blocking performance/memory** with 1-3 M entries and regex-heavy lists; clean-room parser drift | [blocking §5.1] | blocky's structures; CI bench gate; regex cap; CRC-skip rebuilds; conformance corpus cross-checked against AdGuard Home black-box | `blocking.Snapshot`, slow-path bucket |
| 8 | **OIDC/RBAC footguns** (auto-admin, token oracles, CSRF on plain-HTTP labs, IdP quirks) | [auth §10] | strict provisioning; constant-time full-hash compares; CSRF header always required for cookie writes; tolerant claim parsing; dex + Keycloak + Authentik in e2e; security review before v0.1; SAR default (no `impersonate` grant) | `Authenticator` chain, `Authorizer`, `ClaimMapper` |
| 9 | **Two-driver semantics drift** (sqlite vs kube) | tech-lead lens | bounded `Apply` contract (§4.3): whole-object, `managedBy`, optimistic concurrency, no sub-object field-manager emulation; conformance suite is the contract | `store/conformance` |
| 10 | **apiextensions/CEL dependency weight** in the Docker binary | tech-lead lens | **retired** by the validator spike (D75): +12.9 MB on the scaffold binary, +6.7 MB once controller-runtime is linked, +9 MB RSS; the JSON-Schema fallback would save only 2.8 MB and lose defaulting, ratcheting and message parity; a CI binary-size budget step guards growth | `store.Validator` |
| 11 | **miekg/dns v1 end-of-life** | [go-dns-stack §2] | `dns.Msg`/`dns.RR` confined to four packages; migrate when CoreDNS does | record model isolation |
| 12 | **Kubernetes LB realities** (mixed-protocol unsupported on some clouds; `Local` policy imbalance; hostNetwork sysctl limits) | [k8s §9.1] | chart `splitProtocols` + shared-IP annotations, cloud matrix docs, e2e on kind + MetalLB, `trafficDistribution` value | Helm values only |
| 13 | **CRD scale** (thousands of records per zone, 1.5 MiB cap, ~10 updates/s) | [go-dns-stack §6.2, §17.3] | `DNSRecordSet` ≤ 1000 endpoints; status summarises; large/imported/secondary/signed zones are node-local zone data, never CRDs; documented ceilings (~50 k records/zone, ~10 updates/s) with a `doctor` check; escape hatch raft driver (v1.0) | zone data vs zone config split |
| 14 | **Static-twins HA serial skew** confuses external secondaries | §6.5 | documented; dnsdist `order` pins XFR to one node; follower mode removes it | serial rule |
| 15 | **Per-node query logs in k8s** (emptyDir loss) | [observability §2.5] | documented; `persistence.querylog` PVC opt-in; the optional central database keeps every node's query log and statistics (D160); never NFS | `querylog.Sink`, `pgquerylog.Shipper` |
| 16 | **Licence contamination** (code *and* embedded data) | [blocking §1.1, pihole §0] | `go-licenses` deny list; NOTICE; CONTRIBUTING forbids porting from GPL/EUPL repos; supervisor images carry GPL text + source offer (written offer for the from-source keepalived build); the embedded starter blocklist and any future blocked-services/safe-search catalogue are built only from permissively licensed sources with a provenance file checked in CI (§6.4) | — |
| 17 | **Scope creep / bus factor** (Gravity's trajectory; v0.1 is already 16 weeks) | [gravity §15] | milestones with exit criteria; seams table; CONTRIBUTING architecture map; generated clients/docs; features gated on conformance tests | — |
| 18 | **cgo-free constraint** | [go-dns-stack §12] | modernc SQLite only; pure-Go ClickHouse client; `CGO_ENABLED=0` enforced in release | — |

Spike list for weeks 1-2 (before committing the v0.1 schedule): (a) dnsdist 2.1.2 started from YAML + `dnsdist -C cfg -e 'newServer({...})'` and `reloadAllCertificates()`; (b) PROXYv2 + `chashed` + health `validation_function` smoke; (c) kube driver + envtest conformance of `Store`, including `local.db` snapshot restore with the apiserver stopped; (d) blocky matcher port + bench baseline; (e) go-oidc BFF against dex with sealed sessions + `sessionsValidAfter`; (f) mixed-protocol Service on kind + MetalLB and one cloud; (g) apiextensions + cel-go binary-size and build-time measurement; (h) keepalived 2.4.3 `auth_hmac file:` + `track_file` + FIFO on two VMs; (i) quic-go DoQ/DoH3 listeners against `kdig +quic`/`+https` and dnsproxy (D67).

**Spike results (2026-10-11, dev-docs/spikes/):** (c) kube driver passes the conformance suite on envtest with the elector never exiting on lease loss; (d) blocking gates met with margin; (e) OIDC login works end to end against dex 2.46; (g) validator cost measured and accepted, and it found two CRDs a real apiserver refuses (now fixed); (i) all six transports answer through one registry and every PROXYv2 forgery case is dropped. (a), (b), (f) and (h) need a dnsdist binary, Docker/kind and two VMs and remain open.

---

## 15. Open questions for the user

1. ~~**Project home**~~ **Decided 2026-10-10 (D49):** module `github.com/robertherbaugh/kudurru`, images under `ghcr.io/robertherbaugh/`, CRD group `kudurru.io`.
2. ~~**Compose HA ladder**~~ **Decided 2026-10-10 (D50):** v0.1 static twins, v0.2 follower mode with fenced promote, v1.0 raft.
3. **Sessions**: accept sealed cookies in v0.1 (no mid-session IdP group refresh, no session listing), with the SQLite `db` store in v0.2 for Docker and an optional external store (Valkey) for Kubernetes in v1.0?
4. **Kubernetes as data source**: `external` zones (Ingress/Gateway/LB hostnames, v0.2) only, or also replace CoreDNS for `cluster.local` (not before 1.x proposed)?
5. **Blocked-services / safe-search data**: maintain a kudurru-owned permissively licensed catalogue (AdGuard's tables are GPL), or drop those features?
6. ~~**DHCP**~~ **Revised 2026-10-16 (D179, supersedes D42/D52 and D171's original release target):** Opt-in DHCPv4 ships in v0.1.0 on Linux/SQLite with dashboard setup and optional three-node quorum failover. DHCPv6 remains excluded. **Revised 2026-10-17 (D180):** it also runs on the Kubernetes store with `DHCPScope`/`DHCPLease` objects and one elected replica serving relayed clients.
7. ~~**Default block response**~~ **Decided 2026-10-10 (D58):** **Default block response.** `zeroIP` with a 10 minute TTL and EDE 15.
8. ~~**Query-log defaults**~~ **Decided 2026-10-10 (D59):** **Query log defaults.** Privacy `full`, raw rows 30 days up to 10 GiB, answer summary only in `full`, rollups 35 d / 400 d / forever.
9. **Cloudflare ownership marker**: comment only (Free) or comment + tag; also honour/produce external-dns TXT heritage records for coexistence with a parallel external-dns?
10. ~~**UniFi legacy API**~~ **Decided 2026-10-10 (D63):** **UniFi legacy API.** Official Integration API by default; legacy controller endpoints behind a flag with version detection and per-endpoint 404 tolerance.
11. **Edge default in Helm**: dnsdist off by default (proposed) — flip to on once console diff-apply is proven?
12. ~~**Pi-hole API façade**~~ **Decided 2026-10-10 (D66):** **Pi-hole API façade.** Optional module in v1.0.
13. **Transports**: implement DoQ/DoH3 natively in v0.2 (proposed) or rely on dnsdist for QUIC and keep the core smaller?
14. ~~**Minimum hardware/arch**~~ **Decided 2026-10-10 (D68):** **Hardware floor.** Raspberry Pi 4 class (arm64, 1 GB) supported; release targets are linux amd64 and arm64 only.
15. ~~**Authorization in Kubernetes**~~ **Decided 2026-10-10 (D55):** **Authorization in Kubernetes.** SubjectAccessReview by default; apiserver impersonation stays a v0.2 opt-in.
16. ~~**Frontend**~~ **Decided 2026-10-10 (D69):** **Frontend stack.** Bun + React 19 + TypeScript + Astryx (Meta's design system, MIT); uPlot/ECharts for charts until `@astryxdesign/charts` leaves canary.
17. ~~**keepalived build**~~ **Decided 2026-10-10 (D53):** compile 2.4.3 from the upstream tarball; kudurru carries the GPL source offer (D46 stands).
18. ~~**VIP drain policy**~~ **Decided 2026-10-10 (D51):** lameduck keeps the VIP and only drains the node from dnsdist; the VIP moves on real failure or an explicit `kudurru vip drain`.
19. ~~**PROXYv2 authentication**~~ **Decided 2026-10-10 (D56):** **PROXYv2 authentication.** Exact peer allow-list plus the shared-secret TLV for every non-loopback peer; the loopback compose profile is exempt.
20. ~~**RPZ**~~ **Decided 2026-10-10 (D61):** **RPZ.** Import RPZ feeds as a BlockList source in v0.2; publish kudurru's own blocklists as transferable RPZ zones in v1.0.
21. ~~**Encryption-key rotation UX**~~ **Decided 2026-10-10 (D57):** **Encryption key rotation.** Dual-key window via `KUDURRU_ENCRYPTION_KEY_PREVIOUS`; stored secrets are re-encrypted on start and old sessions stay valid until their lifetime ends.

---

## 16. Decision log

| # | Topic | Decision | Why | Alternatives rejected |
|---|---|---|---|---|
| D1 | Base proposal | Start from the MVP-and-risk-first proposal; graft from the Kubernetes-first and single-binary-first proposals | Highest judge tally (24/30 vs 22 and 18); fewest ways to break on both targets; best seam discipline | Kubernetes-first as base (largest v0.1, impersonation default, leader-held zones re-signed on flap); single-binary-first as base (owns consensus from day one, StatefulSet+PVC in k8s, OIDC/dnsdist deferred) |
| D2 | Source of truth in Kubernetes | CRDs via the apiserver are the only store; Deployment, no PVC for config | [k8s §13 model 1, §16.1/§16.2]; zero consensus code; GitOps/RBAC/audit for free | Internal raft store with CRD bridge (two sources of truth, status lag; k8s §16.1); embedded etcd (Gravity's own advice). Note the research disagrees with itself: go-dns-stack §17.3 advises against CRDs as the *primary* datastore (1.5 MiB object cap, apiserver pressure, no home for runtime data); accepted here only for **configuration** objects with the documented ceilings and the zone-data/zone-config split of risk #13 — transferred, imported and signed zone data is never a CRD |
| D3 | Source of truth in Docker | Embedded SQLite (`state.db`) behind the same `Store` interface | One engine for state/log/audit; cgo-free; no fsync-heavy raft log on SD cards in v0.1 | bbolt object store (second engine); raft single-voter from day one (consensus surround is Gravity's bug cluster) |
| D4 | Consensus timing | None in v0.1; `follower` driver v0.2; `raft` driver v1.0 | Validate the product before owning membership/join/snapshots [gravity §16] | raft in v0.1 (single-binary-first proposal); raft "post-1.0 unless demand" (Kubernetes-first proposal) — bare-metal multi-writer is a real user need |
| D5 | Compose HA before raft | v0.1 `ha-static` (identical file-managed config, dnsdist on each node lists all, keepalived VIP); v0.2 follower with fenced promote | Delivers a working keepalived + dnsdist HA profile in v0.1 with zero replication code; follower adds a writable UI without consensus | Remote driver + unfenced `promote` (split-brain by design); no HA until raft |
| D6 | Promotion fencing | `cluster_epoch` + `primary_id` in `state.db`, advertised on `/internal/status`; stale primary demotes itself and returns `503 fenced` | Closes the SRE-identified split-brain in manual promote | No fencing (the Kubernetes-first proposal) |
| D7 | Cold start without apiserver | `zonecache` persists the materialised object set to `local.db`; serve from it with `Degraded`, age-gated readiness, `maxAge` 24 h | Fixes the "DNS needed to bring the cluster up" trap named by the SRE lens | SERVFAIL until informers sync (CRD-only proposals); StatefulSet+PVC with raft (single-binary-first proposal) |
| D8 | Node heartbeats | None stored; Nodes page = EndpointSlice + scatter-gather `/internal/status` | Avoids the Gravity tsdb anti-pattern in miniature [observability §5] | `NodeStatus` in Secrets (MVP-first proposal); per-node Lease (extra writes for little value) |
| D9 | `Apply` semantics | Whole-object apply + `managedBy` single writer + optimistic `resourceVersion`; SSA with field owner in k8s; no sub-object field-manager emulation in SQLite | Bounds the emulation scope a small team can keep correct; makes two sources of truth impossible | Full SSA emulation in ~2-3k LOC (k8s-first, unsubstantiated) |
| D10 | Determinism | Proposer computes uid/timestamps/serial candidates; drivers store verbatim | The v1.0 raft FSM is deterministic without touching callers | Drivers generating values in the apply path |
| D11 | Schema migrations | `SchemaVersion` in every driver from v0.1; newer-on-disk refuses to start; follower halts on version skew; raft `MinVersion` + cluster-min gating | Both CRD proposals had no migration story for the embedded store | Rely on apiserver storage versions only (does not exist in Docker) |
| D12 | Serial rule | `status.serial = max(prev+1, unix_now)`, leader-stamped on `contentHash` change, everywhere | One rule for all drivers; monotonic; near-equal on static twins | Per-commit counter in SQLite only (diverges across twins); hard-coded (Gravity) |
| D13 | Secondary zones | Every replica transfers independently; persisted in `local.db`; no leader in the transfer path | No re-transfer on leader flap (SRE critique of the Kubernetes-first proposal) | Leader as IXFR primary for secondaries |
| D14 | DNSSEC signing (v1.0) | Leader signs; signed RRsets are zone data replicated by IXFR to replicas; new leader continues from the last signed state, never re-signs on takeover | Sequential signers make ECDSA non-determinism irrelevant; no NOTIFY storm on rolling upgrades | Re-sign on every leader change (Kubernetes-first proposal); per-replica signing (go-dns-stack §17.4) |
| D15 | Blocklist distribution | Leader fetches, publishes digest; replicas pull by digest from the leader, compile locally; fallback after 10 min | Avoids N× egress and divergent list versions [k8s §16.11]; v0.1 not v0.2 | Every replica fetches in v0.1 (MVP-first proposal) |
| D16 | dnsdist/keepalived orchestration | kudurru supervisor is PID 1 inside kudurru-published images FROM the upstream digest; render → validate → exec → signal/console | Cleanest day-2 shape; versioned binary, not a shell loop; GPL boundary = mere aggregation [dnsdist §4] | initContainer-copied binary + separate agent container (ambiguous exec path); shell snippet entrypoint from docs (the single-binary-first proposal) |
| D17 | dnsdist membership changes | v0.1 regenerate YAML + restart child; v0.2 console diff-apply gated on the week-1 spike; Lua renderer fallback | Console-under-YAML is UNVERIFIED [dnsdist §1.11]; restart is sub-second behind ≥ 2 edge replicas | Rely on console from day one; Lua `maintenance()` DNS-driven loop (unstable UUIDs) |
| D18 | Edge/VIP in v0.1 | `edge run` and `vip run` supervisors ship in v0.1 | dnsdist and keepalived are hard requirements; both are thin wrappers with the longest real-hardware feedback loop | Static render only in v0.1 (MVP-first proposal); no edge until v0.2 (single-binary-first proposal) |
| D19 | VIP in Kubernetes | `vip.provider: none\|kube-vip\|metallb\|keepalived`, keepalived opt-in only with hostNetwork edge DaemonSet | kube-vip/MetalLB are Service-aware and Apache-2.0 [dnsdist §2.5, k8s §16.14] | keepalived default in k8s |
| D20 | Every dnsdist lists all nodes | Yes, in every multi-node profile | VIP follows dnsdist health, not kudurru health [dnsdist §3.2] | Local-backend-only dnsdist |
| D21 | Kubernetes authz | SAR on CRDs + virtual resources with projected SA tokens in v0.1; impersonation opt-in v0.2 | `impersonate` grant = cluster-admin-equivalent blast radius for an internet-facing server; research vetted SAR, not impersonation [k8s §10] | Impersonation by default (Kubernetes-first proposal) |
| D22 | Dual authz precedence | Allowed if either k8s RBAC (SAR) or kudurru bindings allow; audit names the path | OIDC humans on home clusters rarely share the apiserver's issuer | Single policy system per mode (Kubernetes-first proposal) — leaves OIDC humans without bindings in k8s |
| D23 | Sessions | Sealed cookie in v0.1 (both modes) with `sessionsValidAfter`; `SessionStore.db` v0.2 Docker; external store option v1.0 | Deployment has no shared DB; revocation lever exists | Server-side sessions via raft (single-binary; every session touch a consensus write); refresh token in the cookie (size) |
| D24 | Config metadata / hostOnly | Metadata table with `hostOnly`/`envLocked`/`restartRequired`/`writeOnly` in v0.1 | Fence against the Pi-hole 2026 RCE class; drives UI/docs from one table [pihole §16.8, §17.7] | Defer to v0.2 (MVP-first proposal) |
| D25 | Settings in k8s | Two ConfigMaps: `kudurru-node` (checksum rolls pods) and `kudurru-settings` (hot reload); `Kudurru` CRD in v0.2 | Config changes must not restart pods [k8s §16.6] | One ConfigMap with a checksum annotation |
| D26 | Object kinds | `DNSZone`, `DNSRecordSet` (Endpoint superset), `BlockList`, `ClientGroup`, `Policy`, `Kudurru`, `Integration`, `IdentityProvider`, `RoleBinding`, `ServiceAccount`; `User`/`APIToken`/`TSIGKey`/`DNSSECKey` internal | Mirrors [k8s §12]; keeps hashes out of `kubectl get`; fewest concepts for Pi-hole users | `RRset`/`BlockSource`/`ListGroup`/`Client`/`UpstreamGroup` kinds (single-binary; 15 kinds); `User`/`APIToken` CRDs (Kubernetes-first proposal) |
| D27 | Upstream groups | `Kudurru.spec.upstreams.groups` map referenced by name | One fewer kind; groups rarely change | `UpstreamGroup` object |
| D28 | Record extensions | `Endpoint.kudurru{comment,disabled,expiresAt,view}`; `ToExternalDNS()` drops it | Pi-hole/Technitium users expect comments/disable; external-dns compatibility preserved | Everything in `providerSpecific` (clunky); Endpoint isomorphism strictly (loses UX) |
| D29 | Split-horizon | Labels + per-sink `providerSpecific` + `kudurru.view: public` endpoints + sink `translate` | One zone, one truth [integrations §2.5] | Second hand-maintained public zone |
| D30 | Audit placement | Same SQLite transaction as the mutation in the sqlite driver; `local.db` + stdout + Events in k8s | Atomic audit where kudurru owns the store | Separate `audit.db` only (not atomic) |
| D31 | Permissions source | `internal/rbac/permissions.yaml` generates roles, ClusterRoles, docs, lint fixtures | Prevents drift between two authz systems | Hand-maintained ClusterRoles |
| D32 | Backup format | `kind: List` YAML byte-identical to `kubectl get kudurru.io -A -o yaml`; restore = apply | One format across modes | Tarball of tables (Pi-hole teleporter style) as the primary format |
| D33 | Engine for zone data / snapshots | SQLite (`local.db`), not bbolt | One engine, fewer dependencies | bbolt `zones.db` (Kubernetes-first proposal) |
| D34 | Store validation | CRD OpenAPI + CEL executed locally in Docker via apiextensions packages; binary-size measured in the spike | Same rejects in both modes [k8s §13] | Two validators (JSON Schema in-process, CEL in CRDs; single-binary) |
| D35 | Transports | ~~DoT/DoH in v0.1; DoQ/DoH3 natively in v0.2~~ **Superseded by D67** (all five transports native in v0.1) | "Extreme featureset" without blocking v0.1 on QUIC | Do53-only v0.1 (single-binary-first proposal); all five in v0.1 (Kubernetes-first proposal) |
| D36 | Block response default | `zeroIP`, TTL 10 m, EDE 15 | Responsive to one-click allow; AdGuard default [blocking §6.4] | `refused` (stub resolvers fail over); 6 h TTL (blocky) |
| D37 | Long-term analytics | In v0.1 from rollups | Most-requested missing Pi-hole v6 feature [pihole §17.3] | v0.2 (Kubernetes-first proposal) |
| D38 | Pi-hole API façade | Optional module in v1.0 | Protect v0.2 scope; open question to pull forward | v0.2 (MVP-first proposal) |
| D39 | cluster.local | Forward to CoreDNS; `external` zone type v0.2; `kubernetes` type not before 1.x | CoreDNS-grade conformance burden [k8s §17] | Replace CoreDNS in v0.2 (Kubernetes-first proposal) |
| D40 | Module path | `github.com/robertherbaugh/kudurru` | Confirmed by the git remote; flagged for ratification | `github.com/kudurru/kudurru`, `github.com/kudurru-dns/kudurru` (org names, unverified) |
| D41 | Minimum Kubernetes | 1.30 | CRD CEL validation rules GA (1.29 per the upstream changelog — not covered by the research, verify), ValidatingAdmissionPolicy GA (1.30 [k8s §0]; reserved for optional cross-object policies the chart may ship later), mixed-protocol LB GA since 1.26 [k8s §0] | 1.26 |
| D42 | DHCP | ~~Out of scope for 1.0~~ **Superseded by D171/D179** (opt-in DHCPv4 in v0.1.0) | Gravity's and Technitium's largest bug clusters; UniFi/router owns it | DHCP role in v1 |
| D43 | Extension API | Tier A in-tree Go + Tier B external-dns-compatible HTTP; Tier C go-plugin deferred | Simple integration model that third parties already know [integrations §5.1]; no runtime in-process code | Technitium-style in-process app loading |
| D44 | Leader election lifecycle | client-go `LeaderElector` loop owned by `internal/cluster`; losing the lease stops only leader-only runnables; the process never exits on leadership loss | Stock controller-runtime/kubebuilder behaviour (stop Manager, exit) would take the dataplane down on every pod during an apiserver outage — the opposite of principle 4 [k8s §7.4] | controller-runtime Manager election as-is; per-runnable Leases |
| D45 | PROXYv2 trust | Source allow-list of exact peers **and** a shared-secret TLV (`0xE0`) injected by the dnsdist supervisor; NetworkPolicy template in the chart | Pod-CIDR / LAN-CIDR trust is forgeable by any neighbour and the forged address feeds ACLs, client groups and the query log | CIDR-only trust (dnsdist/PowerDNS default); mTLS between dnsdist and kudurru (dnsdist→backend DoT exists, but client-certificate support for backends is unverified and it adds a certificate lifecycle to every edge pair; revisit if the TLV approach proves insufficient) |
| D46 | keepalived binary | 2.4.3 compiled from the upstream tarball in the kudurru image; 2.3.x distro package supported as a degraded, warned mode | `auth_hmac` (2.4.0+) is the only authenticated VRRP option; Alpine still packages 2.3.4 [dnsdist §2.1] | Alpine `apk` only (no `auth_hmac`); third-party images (unpinned, env-templated) |
| D47 | Internal listener security | Always TLS; token and node certificate derived from the encryption key via HKDF unless overridden | `/internal/snapshot` (v0.2) and `/internal/lists` carry internal kinds and list bodies; a plaintext token listener on the pod/LAN network is a credential and data leak; deriving from the existing shared key avoids a second secret to distribute | Plain HTTP + token (draft v1); separate chart-generated token Secret + cert-manager certs (more moving parts for small installs; still allowed via `tokenFile`/`tls`) |
| D48 | Session payload | Sealed cookie carries resolved role bindings (`roles_snapshot`), not raw IdP groups | Bounded size under the 4 KiB guard regardless of IdP group count; bindings are what authorization needs | Raw `groups` claim in the cookie (draft v1) |

| D49 | CRD group and project home (ratified) | `kudurru.io` is the CRD group; module `github.com/robertherbaugh/kudurru`; images `ghcr.io/robertherbaugh/*`; charts `oci://ghcr.io/robertherbaugh/charts` | Confirmed by the project owner on 2026-10-10 (§15 Q1); a group rename after v0.1 would force a migration on every user | `kudurru.dev`, an org-scoped group |
| D50 | Compose HA ladder (ratified) | v0.1 `ha-static`, v0.2 follower mode with fenced promote, v1.0 raft | Confirmed by the project owner on 2026-10-10 (§15 Q2); keeps Cloudflare/UniFi in v0.2 | raft pulled forward into v0.2 |
| D51 | VIP drain policy | `kudurru lameduck` never FAULTs the VIP: it drains the node from dnsdist's backend pools and the VIP stays while the local dnsdist is healthy with ≥ 1 backend; the VIP moves only on real failure or an explicit `kudurru vip drain` / maintenance (`300`) | Project owner on 2026-10-10 (§15 Q18): routine rolling restarts must not move the VIP, which under `nopreempt` would never come back; "every dnsdist lists all nodes" (D20) already makes VIP ownership follow dnsdist health | lameduck = FAULT (draft v1, the VIP moved on every restart of the MASTER) |
| D52 | DHCP (ratified) | ~~Out of scope for 1.0; candidate for 2.0~~ **Superseded by D171/D179** (opt-in DHCPv4 in v0.1.0) | Project owner on 2026-10-10 (§15 Q6); D42 stood, with a named horizon | DHCP role in 1.x |
| D53 | keepalived build (ratified) | Compile 2.4.3 from the upstream tarball in the kudurru image; GPL source offer shipped | Project owner on 2026-10-10 (§15 Q17); D46 stands | Alpine 2.3.4 package without `auth_hmac` |

| D54 | Sessions (ratified) | Sealed AES-256-GCM cookies in v0.1 (both modes) with `sessionsValidAfter` revocation; `SessionStore.db` on SQLite in v0.2 for Docker; optional external store (Valkey) for Kubernetes in v1.0 | Project owner on 2026-10-10 (§15 Q3); D23 stands; a Deployment has no shared database in v0.1 and the revocation lever exists | db session store required in v0.1; db store for Docker only in v0.1 |
| D55 | Kubernetes authorization (ratified) | SubjectAccessReview on real and virtual `kudurru.io` resources by default; `auth.kubernetes.mode: impersonate` remains an opt-in in v0.2 | Project owner on 2026-10-10 (§15 Q15); D21 stands; the impersonate grant makes a compromised kudurru cluster-admin-equivalent | impersonation by default; dropping impersonation entirely |
| D56 | PROXYv2 authentication (ratified) | Exact /32 peer allow-list **and** the shared-secret TLV `0xE0` injected by the dnsdist supervisor are required for every non-loopback peer; loopback (`127.0.0.1/32`, `::1/128`) is exempt when the secret is unset, so the compose `single-edge` profile needs no secret | Project owner on 2026-10-10 (§15 Q19); D45 stands; CIDR trust alone is forgeable by any neighbour, while loopback inside one network namespace is not | TLV required on loopback too; peer list only with NetworkPolicy and host firewalls |
| D57 | Encryption key rotation (ratified) | `KUDURRU_ENCRYPTION_KEY_PREVIOUS[_FILE]` dual-key window: decrypt with either key, re-encrypt stored secrets on start, sessions sealed under the old key remain valid until `lifetime`; the operator removes the previous key after one session lifetime; `doctor` warns while both keys are set for longer than `lifetime` | Project owner on 2026-10-10 (§15 Q21); zero-downtime rotation on rolling clusters without a second command to learn | `kudurru admin rotate-key` rewriting the store in place with a brief outage; both mechanisms |
| D58 | Default block response (ratified) | `zeroIP` (0.0.0.0 / ::), `blockTTL` 10 m, EDE 15 as the default block response; `nxdomain`, `customIP` and `refused` remain per-policy options | Project owner on 2026-10-10 (§15 Q7); D36 stands; short TTL keeps one-click allow responsive | NXDOMAIN with SOA by default; zeroIP with a 1 h TTL |
| D59 | Query log defaults (ratified) | privacy `full`; raw retention 30 d capped at 10 GiB; answer summary stored only in `full`; rollups `_1m` 35 d, `_1h` 400 d, `_1d` forever; operators dial privacy down per node or per policy | Project owner on 2026-10-10 (§15 Q8); Pi-hole parity out of the box is the product promise | 7 d / 2 GiB default; anonymised clients by default |
| D60 | Blocked-services and safe-search catalogue (revised by D120) | Maintain a kudurru-owned catalogue (blocked services, safe-search rewrites) in a separate permissively licensed data repository, fetched at runtime like any list and seeded at build time with provenance; it ships in **v0.1** milestone M2, not v0.2 | Project owner on 2026-10-10 (§15 Q5); one-click "block TikTok" is part of the Pi-hole/AdGuard-grade promise and AdGuard's tables are GPL | ship in v0.2 (the draft default); drop the features |
| D61 | Response Policy Zones | RPZ feed import (`format: rpz` on a `BlockList` source, AXFR/IXFR-fed) in v0.2; RPZ publishing (serve a compiled BlockList as a transferable zone with NOTIFY to subscribers) in v1.0; the `blocked_rpz` status becomes live in v0.2 | Project owner on 2026-10-10 (§15 Q20); lets kudurru consume BIND/Knot feeds and act as the blocklist source for other resolvers | leave reserved past v1.0; import only |
| D62 | Cloudflare ownership marker (ratified) | Internal ledger + Cloudflare record comment `kudurru:owner=<clusterID>` (Free plan) + tag `kudurru-owner:<id>` when the plan supports tags; external-dns `heritage=` TXT records are read for `adoptFrom` migrations and never written; unowned records are untouched unless `adoptUnowned: true` | Project owner on 2026-10-10 (§15 Q9); §9.4 step 3 stands; works on every plan and coexists with a parallel external-dns | comment only; also writing heritage TXT records |
| D63 | UniFi API surface (ratified) | Official Integration API (`X-API-Key`, `/proxy/network/integration/v1`) by default; legacy `/rest/user`, `/stat/sta`, active-leases and `stamgr` only behind `unifi.legacy: true` with `GET /v1/info` version detection and per-endpoint 404 tolerance | Project owner on 2026-10-10 (§15 Q10); §9.5 stands; aliases, fixed IPs and VLAN detail exist only on the legacy API, and the flag confines upgrade breakage | official API only; legacy on by default |
| D64 | Kubernetes as a DNS data source (ratified) | `DNSZone.type: external` in v0.2 synthesising records from Ingress / Gateway API routes / LoadBalancer Services; `cluster.local` is forwarded to CoreDNS and a `kubernetes` zone type is not on the roadmap before 1.x | Project owner on 2026-10-10 (§15 Q4); D39 stands; replacing CoreDNS means CoreDNS-grade conformance | commit a kubernetes zone type to 1.x; external-dns webhook only |
| D65 | Edge tier default in Helm (ratified) | `edge.enabled: false` by default in v0.1; the default is reconsidered in v0.2 after the dnsdist console diff-apply spike lands | Project owner on 2026-10-10 (§15 Q11); first install is one Deployment; the edge is a one-line opt-in | on by default from v0.1; off permanently |
| D66 | Pi-hole API façade (ratified) | Pi-hole v6 compatible API façade (`/api/pihole/*`) ships as an optional module in v1.0 | Project owner on 2026-10-10 (§15 Q12); D38 stands; protects the v0.2 scope | pull into v0.2; never |
| D67 | Transports (supersedes D35) | DoQ and DoH3 listeners (quic-go, patterned on CoreDNS `server_quic.go` / `server_https3.go`, ALPN `doq`/`h3`, RFC 9250 error codes) ship natively in v0.1 milestone M1 alongside Do53, DoT and DoH; the listener registry is keyed by protocol and all TLS listeners share one cert-reload path | Project owner on 2026-10-10 (§15 Q13); the "extreme feature set" promise applies to transports too; dnsdist stays optional rather than required for QUIC | native DoQ/DoH3 in v0.2 (D35); QUIC via dnsdist only |
| D68 | Hardware floor and release architectures (ratified) | Supported floor is Raspberry Pi 4 class (arm64, 1 GB RAM); release binaries and images for `linux/amd64` and `linux/arm64` (plus `darwin` amd64/arm64 for the CLI); no `armv7` and no `riscv64` targets | Project owner on 2026-10-10 (§15 Q14); keeps the build and e2e matrix to two Linux architectures | armv7; riscv64 best effort |
| D69 | Frontend stack (supersedes the §2/§10 draft stack) | Bun is the web toolchain (package manager, test runner, bundler via `bun build`; Vite under Bun is the fallback if the Bun bundler lacks a feature); React 19 + TypeScript; **Astryx** (`@astryxdesign/core` + a `@astryxdesign/theme-*` package, MIT, consumed as prebuilt CSS so no StyleX/Babel build plugin) replaces Tailwind + shadcn/ui as the component and theming layer; TanStack Query for data fetching; Astryx tables where adequate, TanStack Table + Virtual for the query log; uPlot for the live time series and ECharts for pies/top lists until `@astryxdesign/charts` is published as stable; generated TypeScript client from the huma OpenAPI document; SPA embedded with `go:embed` | Project owner on 2026-10-10 (§15 Q16); project owner's choice; Astryx ships 150+ accessible components, brand-level theming and dark mode, an MCP server and a machine-readable component manifest that keeps agent-written UI code honest. Risk: Astryx is in Beta, so the version is pinned exactly and customisations go through `swizzle` rather than forks | React + Tailwind + shadcn/ui (draft default); Lit + PatternFly; Svelte 5 |
| D70 | Validator and store contract | `store.Validator` entry points `Validate`/`ValidateStatus`/`ValidateUnstructured`; the validator writes defaults back; Apply never changes status, `UpdateStatus` never changes spec; generation moves only on spec change; an identical re-apply is a no-op; the API validates unstructured input before converting | Measured against a real apiserver (envtest) in the validator and kube spikes; the memory and kube drivers pass the suite with these cases and no skips | Typed-only validation (cannot tell absent from zero); the draft memory driver that kept status on Apply |
| D71 | Session key schedule and grants | HKDF-SHA256 per purpose with a 4-byte key id; `roles_snapshot` grants `role` / `role@kind:name` frozen at login; `__Host-` cookie when Secure; single-use pre-auth cookie; group-overage claims refused; IdP usernames prefixed `<idp>:` | Auth spike against dex 2.46: 546-byte cookie for a 200-group user; go-oidc leaves nonce/audience checks to the caller | Raw groups in the cookie (9,210 bytes); one key for every purpose |
| D72 | PROXYv2 peers in Kubernetes | `proxyProtocolFrom` accepts CIDRs only when `proxyProtocolSecretFile` is set; exact `/32`s otherwise; loopback exempt from the secret but must be listed | Pod IPs are dynamic, so the exact-peer rule of D56 cannot express the edge Deployment; the TLV authenticates and the CIDR narrows exposure | Exact `/32`s only (D56 as written); CIDR trust without a secret |
| D73 | Blocking engine specifics | Suffix set is kudurru's own (TLD buckets, bit filter, fence index, shared maphash seed); regexes indexed by required-literal 4-grams, warn above 50 unindexed, hard cap 1,000; RSS gate is steady state after GC in a fresh process; list semantics as recorded in §7.3 | Spike measurements and a 144-expectation corpus; blocky v0.35 has no such suffix structure | "Copied from blocky" wording; linear regex scan; 500-regex warning |
| D74 | cel-go pin | `github.com/google/cel-go` v0.29.2, the version `k8s.io/apiextensions-apiserver` v0.37.1 imports; bumped only with the Kubernetes minor; v0.2 claim mappings use the same module | One CEL copy in the binary | v0.31.x from the draft §2 |
| D75 | Risk #10 retired | The apiextensions + CEL validator cost is accepted: +12.9 MB on the scaffold binary, +6.7 MB with controller-runtime linked, +9 MB RSS; add a CI binary-size budget | Measured in the validator spike | JSON-Schema + bare cel-go fallback (saves 2.8 MB, loses defaulting/ratcheting/parity) |
| D76 | Transport limits | `MaxTCPQueries` unlimited with 10 s idle; UDP read buffer 4096 B (+2 KiB with PROXYv2); DoH body cap 65,535 B, headers 16 KiB; DoQ grants no unidirectional streams; QUIC connection cap configurable | miekg's defaults closed TCP/DoT after 128 queries and dropped EDNS queries over 512 B | 2 MiB DoH cap in the spike brief |
| D77 | Kubernetes RBAC for the kudurru ServiceAccount | create, patch and delete on `kudurru.io/*` (server-side apply needs create+patch), patch on `*/status`, get on namespaces (kube-system uid as the cluster id), leases get/create/update | Least-privilege run in the kube spike | get/list/watch only (draft §5.3) |
| D78 | Elector contract | `Elector(name)` maps to Lease `kudurru-<name>`; integration leases are `kudurru-integration-<ns>-<name>`; `Run` passes the term context to `onStarted`; the kube driver's elector owns the client-go loop and `internal/cluster` runs leader-only work through `Store.Elector` | Kube spike: lease loss never returned from `Run`, at most one concurrent term, 12 s/18 s failover at production timings | Separate lease naming in the draft §5.3; controller-runtime's process-level election (D44 stands) |

| D79 | local.db ownership | zonecache owns local.db: it persists its own materialised object set for any driver after the first sync, then at most every 30 s when changed, refreshes the timestamp every 5 min while live and unchanged, and writes only fully synced live data; `kudurru_store_snapshot_age_seconds` is 0 while live. The code lives in `internal/store/localdb` | One source for the snapshot and the views; a quiet cluster must not age past maxAge | The kube driver's own PersistSnapshots (store-kube spike); write-on-change only |
| D80 | Serving while unsynced (v0.1) | Until zonecache has built its first live or snapshot view every query is answered SERVFAIL, because a zone that has not loaded yet may cover the name. A stale local.db (older than maxAge) is treated the same way in v0.1; the §4.4 refinement (SERVFAIL only for primary zones while forwarding continues) needs a primary-only gate in the authoritative stage and is an M2 follow-up | Never NXDOMAIN or an upstream leak for a name a local zone owns | Forward everything while unsynced |
| D81 | Restore and finalizers | Restore keeps each object's recorded managedBy and uses `bootstrap` only when none is recorded; finalizers are cleared only by Apply (status writes never change metadata), and the Apply that clears the last finalizer removes the object | Matches the kube driver and the apiserver's status subresource | bootstrap as the manager of every restored object |
| D82 | Cacheability of hinted routes | A forward-zone hint to the default group (the bootstrap root zone) is ordinary forwarding and cacheable; hints to any other group and conditional suffixes are never cached | Otherwise the bootstrap root zone disabled the cache for every recursive query | "hinted routes are never cached" as first implemented |
| D83 | Serial stamping cadence | Zone reconciles triggered by record-set changes are coalesced over a 250 ms window, one serial per burst; the serial still moves only when contentHash changes | A bulk import stamped one serial per record set (284 for 300 objects) | Immediate per-event stamping |
| D84 | File-managed object ownership | File declarations take over bootstrap-managed objects and objects of files that dropped them, never `api`, `system` or `integration:*` objects (ManagedByOther diagnosis); first boot means no object of the seeded kinds exists, and a default the operator deleted is not recreated | Files are the operator's intent in Docker mode; API edits must never be silently overwritten | Files always win; always recreate defaults |
| D85 | M1 settings keys | `store.driver` (sqlite\|kube), `store.kube.startupTimeout`, `store.kube.snapshot.maxAge`, `cache.*` (maxEntries, minTTL, maxTTL, negativeTTL, prefetch.*, serveStale.*), `upstreams.ecs.*` (mode strip\|forward\|add, prefixes), `objects`, `objectsDir` | Added by the M1 packages; all in the metadata table with the drift test | — |

| D86 | Client identification tiers | exact IP > MAC (v0.2) > longest CIDR > hostname > nameGlob > clientID > catch-all `/0` > default group; equal-length CIDR ties join every tied group and add a Tie diagnosis | The bootstrap default group is two `/0` CIDRs; under a literal "CIDR before hostname" order every hostname matcher would be dead | Literal §7.1 order |
| D87 | Policy merge | Lists are united across active policies (allow lists first); each setting comes from the highest-priority policy that sets it; `allowOnly` from the top policy; equal priorities ordered by namespace/name; the bootstrap `Policy/default` is the fallback; a policy `upstreamGroup` replaces only the default-group hint and its answers are not cached (D82) | Deterministic merge for Pi-hole-style overlapping groups | Last-writer-wins |
| D88 | Block response precedence and shape | BlockList `response` > Policy `blockMode` > `blocking.defaultResponse`; `nxdomain` carries no SOA; EDE 15 for list blocks and 17 for allow-only with the list name as extra text; statuses blocked_exact/blocked_list/blocked_regex/blocked_cname by matched rule kind; `pipeline.ErrDrop` and a nil-Msg Response with status dropped/blocked_rate_limit mean "send nothing" on UDP and SERVFAIL elsewhere | Measured 0.4-0.7 µs stage overhead; drops must not be answered | NXDOMAIN with SOA (draft §7.1) |
| D89 | Transfers and NOTIFY | `zoneTransfer` is the address mode and, when set, `transferTSIGKeyRefs`; empty means deny; AXFR over TCP/DoT only; IXFR answered with AXFR (single SOA when current) until the journal exists; NOTIFY signed with the first transfer key, 5 attempts with backoff, targets = `notifyTargets` and/or apex NS except MNAME; invalid transfer/notify policy degrades the zone, invalid `queryACL` still stops it | RFC 5936/1995/1996/8945 behaviour verified end to end with miekg clients; failing open would leak a zone | UDP AXFR; re-transfer on every leader flap |
| D90 | contentHash and endpoint identity | `status.contentHash` = `zone.ContentHash` of `zone.Build(zone.ServedRecordSets(...))`, shared by the controller and the zone cache; identity is (owner, type, setIdentifier); CNAME conflicts are Duplicated across objects and Invalid within one; data below a DNAME is Occluded | Hash parity between what replicas serve and what the serial tracks | Tuple hash in the controller (M1) |
| D91 | D80 as implemented | Unsynced: every query SERVFAIL except positive data from loaded zones; stale local.db: SERVFAIL for names under primary zones, forward zones and other names forwarded; new status 29 `servfail_unsynced`, Upstream `zonecache:unsynced` or `zonecache:stale` | Closes the M1 carry-over | Forward everything while unsynced |
| D92 | Internal listener credentials (D47 detail) | HKDF-SHA256 info strings `kudurru/internal/v1` (token) and `kudurru/internal-ca/v1` (CA seed); fixed verification name `internal.kudurru.invalid`; TLS 1.3 only; during rotation nodes issue from the previous CA while `_PREVIOUS` is set and clients retry the previous token on 401; `listeners.internal.tls` is four optional file keys | Two in-process nodes verified; no extra Secret in the chart | auto-derived certs with no rotation story |
| D93 | List pipeline specifics | A body is refused only when invalid lines exceed `maxErrorsPerSource` and outnumber its rules (browser filter lists have many cosmetic rules); replicas keep serving an older cached body while a newer digest is pending; the 10-minute self-fetch fallback also starts when no digest was ever published; the embedded starter list is Steven Black's own MIT list (2,852 names), never the unified file | Measured: 1 M-entry pull 90-237 ms, leader compile 0.7 s | blocky's literal error threshold |
| D94 | Rate limiting placement | The per-prefix QPM limiter runs first in ingress, before the FORMERR/NOTIMP/BADVERS checks, so floods of malformed queries are charged too; TCP is never limited; loopback and the health name are exempt; over-limit UDP gets TC=1 for `slip` percent then drops | 127 ns per check, 0 allocs | Limiter after validation |
| D95 | local.db layout version | local.db carries `meta.layout_version` (2: blobs table for the cache dump) separate from `meta.schema_version` (the object schema); zonecache is the only writer of the object snapshot, cache/persist writes the blob | Different change cadence from the object schema | One version for both |
| D96 | Query log sizing | Measured 141 B/row with privacy `full` (116 B without the answer summary), 28-31 k rows/s write capacity, so the 10 GiB default holds about 8 days at 100 qps of raw rows; rollup ts is unix seconds of the UTC bucket; proto 6 is doh3; dictionary keys prune only after 400 days unseen; pseudonym mode rotates a random key at UTC midnight | Replaces the ~100 B/row estimate in §11.2 | — |
| D97 | M2 settings keys | `blocking.{enabled,defaultResponse,blockTTL,allowDegraded,customIP.v4,customIP.v6,refreshInterval,initStrategy,listsDir}`, `querylog.{enabled,sink,privacy,anonymize,retention.raw,retention.maxDBBytes,exclude.*}`, `listeners.internal.{address,tokenFile,tls.*}`, `listeners.sockets`, `ratelimit.*`, `tsigKeys[]`, `notify.source` | All in the metadata table with the drift test | — |

| D98 | API server specifics | huma v2 over the Go mux; Scalar docs at `/api/v1/docs`; `api/openapi.json` (3.1) and `api/openapi-3.0.json` committed with a drift test; the generated Go client lives in `internal/apiclient/openapi` (the CLI's hand-written transport in `internal/apiclient` wraps it later); own-token routes are authenticated-only (a zone-scoped binding cannot satisfy a cluster-scope `tokens.own`); `POST /domains` attaches the `ui-allow`/`ui-deny` lists to `Policy <ns>/default` when that policy is API- or bootstrap-managed; `/dns/query` needs `stats.read`, `/info/messages` and `GET /node/config` `settings.read`, `PATCH /node/config` `node.config`, `/nodes` and `/cluster` `cluster.read`, `/edge` `edge.read` | Measured 3.5-9 ms for `GET /zones` with 1,000 zones through the full chain | `/api/docs` (draft §3.4); route-level `tokens.own` |
| D99 | Authorization principal | The API's Authorize step uses `auth.RBACPrincipal`: builtin-admin flag, session or token grants, implicit grants and token scopes reach the engine; `Decision.Path` is `local`, `sar`, `binding` or `none`, with `local` covering the first-boot admin, the bootstrap and break-glass service accounts, the unix-socket peer and a peer node's implicit viewer+auditor grants | Integration found the first-boot admin forbidden everything when the API mapped only name and groups | Name-and-groups mapping (draft brief) |
| D100 | Internal kinds | `LocalUser`, `APIToken`, `ServiceAccount`, `RoleBinding`, `IdentityProvider` live in `api/internalkinds/v1alpha1` (Go's internal-package rule forbids `api/internal`), are never CRDs, and in Kubernetes are Secrets named `kudurru-<kind>.<ns>.<name>` labelled `kudurru.io/kind` with the object JSON in `data.object`; the release-namespace Role therefore needs `delete` on secrets | Both drivers serve them through the same `Store` interface; envtest-verified | CRDs for IAM kinds (Kubernetes-first proposal) |
| D101 | Auth behaviours pinned by the stack | Admin socket is HTTP/1.1 JSON on `auth.adminSocket.{path,group}` (`/admin/v1/reset-password`, `create-token`, `list-users`; peers by SO_PEERCRED uid 0, the server's uid or the group); a CSRF failure on a cookie request drops the session (403 on protected routes, public routes run anonymous); bearer OIDC ID tokens are accepted in v0.1 and access tokens refused; a token may not create a token with wider scopes or a later expiry than its own; `POST /auth/password`; `localLogin: admins-only` admits builtin admins and cluster-admin role holders; sessions issued after a password change carry `iat = sessionsValidAfter + 1 s`; `auth.kubernetes.mode` accepts only `sar` | Measured 2-6 µs per authenticated request; dex e2e green | NDJSON admin protocol (draft brief) |
| D102 | Dashboard conventions | `comment` and `enabled` on every object are the annotations `kudurru.io/comment` and `kudurru.io/enabled="false"` (servers do not honor `enabled` yet, see followups); React 19.3 + Astryx 0.6.x with `web.Handler` setting its own CSP; a placeholder `dashboard/dist/index.html` is committed so `go build` needs no Bun and `make dashboard-build` produces the real bundle (235 KiB gzip initial, 600 KiB budget enforced) | Keeps the Go build self-contained and the bundle bounded | Spec fields for comment/enabled (needs a v1alpha1 change) |
| D103 | CLI conventions | Own config at `~/.config/kudurru/config.yaml` with named contexts and `tokenStore keyring|file`; persistent `-n/--namespace` and `-o/--output`; `kudurru login` mints a personal token through the session; `kudurru admin` speaks the HTTP admin socket; `--context` always names a kudurru API endpoint in v0.1 (kubeconfig passthrough is a follow-up) | Matches kubectl muscle memory without pretending to be kubectl | kubeconfig contexts in v0.1 |
| D104 | Supervisor images | tini is PID 1 and the kudurru supervisor its only child (CMD, so compose `command:` overrides work); the VRRP check is `kudurru health --vrrp …` as §3.6 spells it (rendered by default since the M5 burndown, e8b36b4; `kudurru vip probe` is the same probe with identical flags); `vip fifo` publishes state to `/run/kudurru/vip-state.json`; `kudurru vip drain <vip> [--undo]` is the explicit way to move a VIP and lameduck can never write 300; the PROXYv2 secret must be printable ASCII of at least 32 bytes because dnsdist YAML cannot carry bytes; candidate files are `<base>.candidate.<ext>`; `keepalived_script` is uid/gid 953 so the probe can read the dnsdist API key | Renderers golden-tested; real `dnsdist --check-config` and `keepalived -t` need `make edge-check` on a Docker runner | Supervisor as PID 1 (§3.6 wording; the GPL boundary is unchanged) |
| D105 | Deployment shapes | Node file at `/etc/kudurru/node/kudurru.yaml`, settings file at `/etc/kudurru/settings/settings.yaml` (merged into the node file until a settings reader with hot reload exists); `preStop: sleep` instead of `kudurru lameduck`; the `net.ipv4.ip_unprivileged_port_start=0` sysctl instead of a file capability for :53 as non-root (hostNetwork pods still need the capability); `mode=kubernetes` with `persistence.querylog` or `persistence.local` becomes a StatefulSet with per-pod claims; seed objects are post-install hook resources; with `vip.provider=keepalived` keepalived is a second container in the hostNetwork edge DaemonSet; CRD chart copies carry `helm.sh/resource-policy: keep`; kustomize bases are rendered from the chart and drift-tested; the `-debug` image is distroless `debug-nonroot` (no dig) | 33 chart permutations and 7 kustomize builds validated with kubeconform against Kubernetes 1.30 | Separate keepalived DaemonSet; `kudurru apply` seed Job |

| D106 | Settings file layer (D25 implemented) | Layer order is defaults, node file, settings file (`settingsFile` / `KUDURRU_SETTINGS_FILE`, cluster-scope keys only), environment; the file is watched with fsnotify plus a 10 s poll; `PATCH /settings` persists into it in Docker and answers "managed by Kubernetes" with the kube driver; hot keys: upstream groups and conditional map (set swap + cache flush), cache limits, blocking defaults, ratelimit, querylog privacy/retention, log level; `restartRequired` in the metadata table is now truthful and pinned by a test | The chart mounts the settings ConfigMap and sets the variable; settings changes no longer roll pods | Merging settings into the node file (the M4 chart) |
| D107 | Lameduck (D51 drain path) | `POST`/`DELETE /lameduck` on the ops listener, loopback peers only; while lameduck the health name answers SERVFAIL so dnsdist's `must_resolve` check drains the node, readiness is 503, queries are still answered; `kudurru lameduck [duration] [--cancel]` is the client and the chart's preStop | Verified live: health SERVFAIL while other names answer, ready 503, --cancel restores NOERROR | admin-socket verb; preStop sleep |
| D108 | Silent drops on every transport | A control-stage or rate-limit drop sends nothing on UDP and DoQ, closes TCP and DoT, aborts DoH and DoH3; the log observer labels such responses `rcode="none"` | Answering SERVFAIL to a flood costs more than the query | SERVFAIL over TCP (M2) |
| D109 | `kudurru.io/enabled` | `kudurru.io/enabled: "false"` is the v0.1 switch for BlockList, ClientGroup and Policy, honoured by the policy index, the list resolver/fetcher and the controller (`Disabled` condition); `spec.enabled` fields were rejected to keep the external-dns Endpoint superset untouched | Dashboard "enabled" toggles now take effect | spec fields |
| D110 | Credentials never outlive their account | Sessions and API tokens are refused when issued before the local user or service account they name was created (`auth.Principal.CredentialIssuedAt`); the kube driver restores API tokens after their owners; per-IP login lockouts key IPv6 clients by /64 and count the real client behind trusted proxies | Security review S2, S3, S10 | Name-keyed credentials |
| D111 | List URL guard | BlockList URLs may not resolve to loopback, link-local, unspecified, multicast or known cloud metadata addresses unless `blocking.allowLocalListURLs` is set; private LAN ranges stay allowed; the guard checks the resolved address at dial time | Security review S1 (SSRF from `blocklists.write`) | Unrestricted URLs |
| D112 | Webhook and stream limits | The external-dns webhook listens on `127.0.0.1:8888` by default and warns when exposed; at most 64 concurrent live query streams per node with a 30 s write deadline; forwarded auth bodies are capped at 64 KiB; the built-in dig (`POST /dns/query`) marks its requests synthetic and they never reach the query log; `hide_domains` also hides the matched zone | Security review S4-S8 | Unbounded defaults |
| D113 | Renderer input validation | dnsdist topology strings are validated before rendering (hosts are IP addresses or DNS names; cert, key and DoH paths are printable ASCII without quotes or spaces; no newlines anywhere); keepalived paths allow braces only as the literal `{vrid}` | Security review S9, e2e finding E2E-6 | Quoting only |
| D114 | Leader address in Kubernetes | `cluster.ElectorMembership` resolves the Lease holder's pod through the headless EndpointSlice (30 s TTL) and falls back to the DNS name | `dnsPolicy: Default` pods cannot resolve `<pod>.<headless>` | DNS only (M2 wiring) |
| D115 | Internal zones route and serial metrics | `GET /internal/zones` returns `{node, zones:[{name, serial, type}]}` from the zone cache; `kudurru_zone_serial{zone}` and `kudurru_zone_records{zone,kind}` exist, so the serial-divergence panel and alert can ship | The edge supervisor polls it to flush the authoritative packet cache | — |
| D116 | Aggregated ClusterRoles are generated | `make rbac-manifests` generates `deploy/helm/kudurru/templates/rbac-aggregated.yaml` from `internal/rbac/permissions.yaml` (D31 implemented) with a drift test; `kudurru-auditor` is no longer aggregated into the built-in `view` role because that would expose query logs to every viewer | Hand-written roles had drifted (custom verbs flush/toggle/refresh) | Hand-maintained ClusterRoles |
| D117 | Release identity | A build without git metadata reports `RELEASE_VERSION` (0.1.0-rc.1); charts are at 0.1.0-rc.1; the binary-size budget is re-baselined to 60,362,000 bytes after the dashboard, Scalar bundle (self-hosted under the strict CSP, +1 MB) and supervisors; `dev-docs/release-checklist.md` lists the owner-hardware steps (edge-check, kind + MetalLB, two-VM VRRP) | Measured 54.9 MB stripped | 51.2 MB budget from M1 |
| D118 | E2E and fuzzing are opt-in but real | `make e2e` (KUDURRU_E2E=1) drives real `kudurru serve` binaries: BIND 9.18 as a secondary (AXFR, NOTIFY, TSIG refusal) and as a conditional upstream, a two-node internal-listener test, SIGTERM lameduck, restart continuity, config and port failures; `make fuzz` runs every Fuzz target with a committed seed corpus; CI runs both (bind9 via apt). Recorded bugs are skipped with `KnownBug` and listed in followups.md; E2E-1, E2E-3, E2E-4 and E2E-5 were fixed right after M5 (`node.lameduck`, default-group hints falling through to conditional forwarding, readiness gated on the first live snapshot, truthful `Generated`) | Single Linux host coverage without Docker | Waiting for Docker-based e2e |
| D119 | Post-M5 hardening before the v0.1.0 tag | The five e2e bugs and E2E-6 are fixed (`node.lameduck` drain window default 2s, default-group hints fall through to conditional forwarding, readiness waits for the first live snapshot, `Generated` only for a created key, braces in keepalived paths only as `{vrid}`); the local edge resolver cache is flushed on list snapshots and toggle changes (`Toggle.SetOnChange`); the chart no longer merges `config.settings` into the node file and the compose files set `KUDURRU_SETTINGS_FILE` on the data volume; a busy admin socket is a warning; the Tools page reads `GET /doctor`; the release workflow builds and signs the supervisor images | Every item had a failing test or a documented gap in followups.md | Carrying them as known limitations into v0.1.0 |
| D120 | Blocked-services and safe-search catalogue (D60 outcome) | Shipped in v0.1 as an embedded, Apache-2.0, kudurru-authored catalogue (`internal/listdata/catalogue`) with per-entry provenance checked in CI. Services compile as block lists `service:<id>`. Safe search answers the vendor's documented CNAME target (or address) with status `blocked_safesearch`. `blocking.catalogue.url` replaces the catalogue at runtime with a schema-version-1 document; the leader fetches it and replicas pull it over `/internal/catalogue`. The separate public data repository is deferred until the project owner creates it (publishing is the owner's call). It landed in the v0.1 fix round, not in M2. Contract: docs/deploy/blocked-services.md | M2 shipped without the catalogue and no decision recorded the slip (only dev-docs/milestones/m2/policy.md said "v0.2"); an embedded copy works offline and the fetch URL keeps the D60 update path without a release | Ship it in v0.2; wait for the data repository before shipping anything |
| D121 | Docs site (M5 exit criterion) | A Hugo site under `docs-site/` is built from the markdown in `docs/` (front matter only where needed) with a CI job; it is implemented in the v0.1 fix round. M5 closed without it | The §13.1 M5 criterion and §12 list a docs site; the markdown is the single source, the site only renders it | A separate docs tree; dropping the criterion |
| D122 | M4 report location | M4 (deploy + edge) was implemented together with M3 and reported in dev-docs/milestones/m3/ (`dev-docs/milestones/m3/README.md` "Milestones M3 and M4", deploy.md, edge.md); there is no docs/m4/ | The serve wiring of both milestones landed in one change; a split report would have duplicated it | An empty docs/m4/ pointing back |
| D123 | Dashboard tested against the real server | The dashboard is tested end to end with Playwright against the real `kudurru serve` binary (`dashboard/e2e`, `make dashboard-e2e`, CI job `dashboard-e2e`); the vitest + MSW unit tests remain for component coverage; the generated OpenAPI types (`bun run gen:api`) are the static half of the guard | Until the v0.1 fix round the SPA was only tested against mock handlers and drifted from the API in ways no test saw | Mocks only; a Go + headless Chrome harness |
| D124 | `Policy.spec.blockedServices` | A pattern-checked string set, not a CRD enum; unknown IDs are skipped and reported as `Degraded{UnknownService}`, and re-checked whenever the catalogue changes | The catalogue can change at runtime (`blocking.catalogue.url`) without a CRD upgrade | An enum of service IDs in the CRD schema |
| D125 | Safe search is part of filtering | `blocking.enabled=false` and the temporary blocking disable also turn safe search off, and an allow-list match skips it | One switch and one allow path for everything the filter rewrites | Safe search independent of blocking |
| D126 | `safeSearchMode` | `strict` (default) or `moderate` on the Policy; the second level exists only because YouTube documents two Restricted Mode levels | Follows the vendors' documented targets instead of inventing levels | A per-vendor mode map |
| D127 | Forwarded auth routes are documented by checked mirrors | The auth stack serves the read-only `/iam/*` lists like `/auth/*` (`api.Options.IAM`/`IAMReader` removed, one implementation). The OpenAPI schemas of every forwarded route are mirror types in `internal/api/authtypes.go`; `TestForwardedShapes` (cmd/kudurru) fails when a mirror differs from the stack type, when the real handler rejects a body built from the documented type, or when a real answer has an undocumented field. API permission routes answer 403 `password_change_required` like the stack's; `/auth/me` reports live grants (local sessions carry no roles snapshot, D48) and a scoped token reports its scopes | The OpenAPI document described token and status bodies the handlers rejected, so a generated client could not work | Wiring a second IAM reader in the API package; leaving the forwarded routes undocumented |
| D128 | Dashboard wire types are generated | The SPA aliases the generated OpenAPI schemas (`dashboard/src/api/generated.ts`, `bun run gen:api`) and the MSW mocks serve the real shapes (settings schema regenerated from a live server). Upstreams are edited on a dedicated page through `PATCH /settings` merge patches; v0.1 has no separate upstream API | Hand-written types drifted from every second route (D123) | A separate `/upstreams` API; hand-written types with a contract test |
| D129 | Stream write deadline | Each message written on a TCP or DoT connection, zone-transfer streams included, gets a deadline of `Limits.StreamWriteTimeout` (default 30 s); a failed transfer write closes the connection | The kernel wakes a blocked writer only after two thirds of the send buffer drained, so the 2 s `WriteTimeout` would cut off a slow WAN secondary | One deadline for the whole transfer; reusing `WriteTimeout` |
| D130 | Object size cap in every driver | `store.MaxObjectBytes` (1.5 MiB, the etcd request cap) applies in the sqlite and kube drivers; both report `store.ErrTooLarge`, which the API maps to 413 `too_large` | An object accepted by sqlite must also fit the kube driver, so a backup restores in either mode | Enforcing the cap only in the API |
| D131 | Dashboard theme and dialogs | The default theme "Kudurru" is Astryx Neutral with the branding palette as CSS custom-property overrides (Paper/Ink surfaces, Clay accent, Clay as text adjusted for AA, Stone dividers); query status colors are kudurru's own light/dark pairs, the same in every theme and separate from the brand; hover and selection are translucent overlays. Every modal uses `components/FormDialog` (header with a close button, scrolling content, footer that stays visible); the close button and Escape always close, the backdrop only until the form was changed, and the forced password change cannot be dismissed | The placeholder look ignored the brand, solid hover tints made rows unreadable, and long dialogs hid their buttons | Astryx Stone as the base (cool grays and washed-out status colors clash with the brand, loads lazily); per-page dialog markup |
| D132 | Pages, wizards and dialogs | Every object a user configures or inspects has a dedicated page with a current-state header and sections edited in place; creating anything with several concepts is a wizard on its own route (`/…/new`, components/Wizard on the Astryx Stepper); FormDialog only for short single-purpose actions. Closed sets, durations, bounded numbers and object references are typed controls, never free text (dev-docs/milestones/m3/ui.md "Pages, wizards and dialogs") | Long scrolling modal forms hid their buttons and context, and free text invited invalid values | Modals for everything; generic object forms |
| D133 | Typed settings metadata | `config.Key` carries label, group, enum (value/label/help), min/max, presets, format, ref and nested shapes; `Config.Validate` enforces it before the cross-key checks, so the file, the environment, the API and the UI share one definition of a valid value; 400 `invalid` answers carry `error.fields` and the allowed values. Closed sets owned elsewhere (rbac roles) are pinned by drift tests | The UI needs the closed sets to offer dropdowns, and duplicated hand-written checks drifted | Enums only in the SPA; JSON Schema per key |
| D134 | Catalogue logos | Logos for catalogue services, safe-search engines and upstream providers are SVGs from homarr-labs/dashboard-icons (Apache-2.0, pinned commit, PROVENANCE) embedded in the binary, sanitised and scanned in tests, served publicly at `/api/v1/catalogue/icons/{slug}.svg` with an ETag; entries without a logo get a monogram | A self-hosted DNS server should not make the browser fetch from a CDN; static logos need no session | A CDN; Simple Icons |
| D135 | Block page | The `blockPage` response answers A/AAAA with `blockPage.ipv4`/`ipv6` (default: the first specific listen address; refused when none) and other types NODATA with EDE 15, falling back to zeroIP when the page is off. `internal/blockpage` serves the page on the node with a strict CSP, rendered by hand (executing html/template disables the linker's dead-method elimination: +12.6 MB; `TestNoTemplateExecution` guards it). The reason comes from the query-log tail, then querylog.db, within the privacy mode, then the client's current policy; the admin preview shows an example reason so `settings.read` reveals no query-log data. HTTPS is optional and always warns; HSTS sites cannot show it | Users of a filtering resolver need to see why a site is blocked, like Technitium's blocking page | Proxying the site; a per-client cache outside the privacy mode |
| D136 | Unblock requests | Requests are `UnblockRequest` objects (namespaced; a CRD in Kubernetes, rows in state.db) indexed per node from a store watch, so the unauthenticated block page never reads the store per view; its writes are bounded per client, per node and by a pending cap, with a same-origin check. Approvals reuse the UI allow lists (`ui-allow` for everyone, attached to the default and requester policies; `ui-allow-<group>` for a group) and record the entry in `status.allow`, so the leader removes exactly what a time-limited approval added. No per-client scope because policies bind to ClientGroups. Clients are stored as `querylog.privacy` allows | One place for admins to fix what blocks users, visible on every node and auditable with kubectl | Node-local storage; a per-client ClientGroup per approval |
| D137 | Per-source list switch and policy attribution | `BlockList.spec.sources[].enabled` (omitted = true): a disabled source is not fetched or compiled, keeps its cache and last status; an all-disabled list is Ready with 0 entries; the whole-list switch stays the `kudurru.io/enabled` annotation (D109). querylog.db schema 2 records the deciding policy per row (additive, upgraded in place) for `GET /policies/{ns}/{name}/state` and `GET /queries?policy=` | Pi-hole-style per-adlist switches; a policy page needs its own activity | Deleting sources to disable them; per-entry switches (the inline format stores none) |
| D138 | Binary-size ceiling | `make size` fails above 100,000,000 bytes (stripped, CGO_ENABLED=0, linux/amd64); the v0.1 UX round measures 58,523,808. Growth is still explained in the change that causes it, and `TestNoTemplateExecution` keeps template execution (which disables dead-method elimination, +12.6 MB) out of the binary | The project owner's call: features over the old 10 % headroom, under a hard 100 MB ceiling | Re-baselining at +10 % per release |
| D139 | Release branches | Releases are promoted through branches: pull requests into `main` push `ghcr.io/robertherbaugh/kudurru:dev-<PR>` (overwritten by later pushes); each push to `release-candidate` (merges from `main` only) tags `v<VERSION>-rc.<N>` and publishes a GitHub pre-release with `<VERSION>-rc.<N>` images; each push to `release` (merges from `release-candidate` only) tags `v<VERSION>` and publishes the GitHub release with `<VERSION>` and `latest`. VERSION in the repository root is the only hand-maintained version; `hack/release-tag.sh` derives the tags, refuses a VERSION that is already released and a release whose tree differs from its last candidate. The workflows push the tags themselves and call `publish.yml` (goreleaser, charts, supervisor images) | A tag pushed by hand could ship an untested commit; a release now ships exactly what its last candidate shipped, and the version lives in one reviewed file | Tags pushed by hand (the previous `v*` trigger); retagging the rc image as the release (the binary would report the rc version); semantic-release from commit messages |
| D140 | Dashboard themes are Astryx themes (revises D131) | `dashboard/src/themes` holds Kudurru (`defineTheme` extending Neutral; colors are stops of a palette generated from branding by `astryx theme palette generate`, brand colors as exact anchors) and Neutral/Stone/Y2K as local themes extending the package themes, all built with `astryx theme build` and wired through the generated `src/astryx-themes.ts` and `<Theme>`; `@astryxdesign/core/reset.css` is imported. Query status colors are theme tokens (`--q-*`, the same in every theme); the high-contrast toggle re-points them to `--q-*-strong`. Themes name only system fonts. The v0.2 Enterprise branding exception uses documented runtime child themes for bounded custom accents, preserving shipped styles and semantic status colors. `astryx doctor` reports no warnings | Astryx's rule against `:root` palette overrides; the packages' Google Fonts imports are blocked by the CSP | Package themes imported as-is; `:root` overrides (D131) |
| D141 | Form fields: label, control, help, status | Every control is an Astryx component behind `dashboard/src/components/fields/` (SelectField, TextField, TextAreaField, NumberField, SwitchField, CheckboxField, EnumSelect, DurationField, ObjectRefPicker, …). Astryx `description` sits between label and control with no placement option, so the wrappers render the help as supporting Text after the control, linked by aria-describedby (re-applied because most Astryx inputs overwrite it), and errors as a detached FieldStatus; Switch and CheckboxInput keep their own description. Raw form elements and `description` on Astryx inputs are lint errors outside `components/fields/`. NumberField checks min/max itself so out-of-range input gets an inline error instead of NumberInput's silent clamp. The dashboard follows dashboard/AGENTS.md (`astryx init --features agents`) | Rows with and without help text did not align, native selects did not match the design system | Astryx Field description above the control; native controls |
| D142 | Creation wizards are modals (revises D132) | Creating anything with several concepts is a wizard in a modal over the page the user is on (`WizardDialog`, composed like Astryx's form-wizard-dialog template: title, ✕ and a compact Stepper pinned above the step, Cancel/Back/Next/Create pinned below). ✕, Escape and Cancel ask "Discard the new …?" (AlertDialog) once anything was entered; the backdrop never closes a dirty wizard; Create closes it and opens the new object's page. List pages open it with `?new=1`; `/…/new` redirects there. A wizard opens no other dialog besides the discard question. Objects keep dedicated pages | A dedicated page for a creation flow took the user away from the list they were adding to | Wizard on its own route (UX round 2); nested dialogs |
| D143 | Allowlist/Denylist storage and per-entry switches | Per-entry state lives on the lists. The `ui` source of `ui-allow` / `ui-deny` (everyone) and `ui-allow-<group>` / `ui-deny-<group>` (one ClientGroup, in its namespace) holds `spec.sources[].entries: [{value, type: exact\|wildcard\|regex, enabled, comment, created, createdBy, modified, origin, request}]`; everyone lists are referenced from every API- or bootstrap-managed policy of the namespace, group lists from the writable policies naming the group, re-checked by the leader every 30 s and on every write; file- or integration-managed policies are reported as `unattached`, never edited. Single entries of any source (URL, ConfigMap, inline) are switched off with `spec.sources[].exclude`, kept across refreshes, with `staleExcludes` in status. Old inline `ui` lines are migrated losslessly once by the leader. `/allowlist`, `/denylist` and `/domains` serve the entries; the query log, unblock approvals and the compatibility `/domains/{allow\|deny}/{exact\|regex}` routes write them | The project owner's choice to keep the state on the lists (with a link back to each list); Pi-hole/Technitium-style allow and deny pages with switches, comments and group scope | A `DomainRule` kind (new CRD, rows, controller, RBAC; one object per entry in etcd); query-time scope in the policy engine |
| D144 | List exceptions on the Allowlist and Denylist; switches on the right | Switched-off entries of non-ui lists (`spec.sources[].exclude`) are shown as `exception` rows on the Allowlist (block lists) or Denylist (allow lists), and on Domains; they have no storage of their own: the row's switch, delete and batch edit the exclusion, and `POST {values, list, source?}` creates them ("Only in list …"). Per-row switches sit on the right, before the actions, in every table; selection checkboxes stay leftmost | One central place for everything that lets a name through, without opening each list; the project owner's layout preference | Exceptions only in each list's entries browser |
| D145 | Catalogue categories and category blocking (revises D120, D124) | Catalogue schema 2 adds categories (stable id, name, description, Lucide icon, order) and per-service `requires`/`related`/`note`; version 1 documents are still read with the embedded category names. `Policy.spec.blockedCategories` blocks every service of those categories in the catalogue in service, including ones a later catalogue adds; `spec.allowedServices` are exceptions inside blocked categories only (never cancelling `blockedServices`; allow lists still win). Unknown categories are `Degraded{UnknownCategory}`. The embedded catalogue has 150 kudurru-authored services in 14 categories plus `other`, with per-entry provenance and a CI check that no service blocks an upstream resolver or a shared sign-in host | Owners block a whole kind of service (AI) without picking each one, and keep up as services appear | A CRD enum of categories; copying AdGuard's or HaGeZi's service lists |
| D146 | Content categories from the Block List Project | Its topic lists (gambling, adult, piracy, phishing, malware, …) are offered as well-known lists and as policy "content categories": each is a BlockList `blp-<id>` with the upstream URL, created on demand in the policy's namespace and refreshed on the normal schedule; nothing is embedded. Recorded licence: repository Unlicense, list headers MIT, the gambling and porn lists merge HaGeZi (GPL-3.0); the UI says so and warns about the size of the malware list | Long-tail categories cannot be hand-written per brand | Embedding the lists |
| D147 | Opt-in runtime import of AdGuard's HostlistsRegistry services | `blocking.catalogue.adguardServicesURL` (default off, restart) makes the instance download AdGuard's GPL-3.0 services file; kudurru ships, vendors and tests no AdGuard data (the format is implemented from the registry's public README; tests use a hand-written fixture). The leader fetches it on the catalogue schedule, replicas pull `/internal/catalogue/adguard`, the last good copy is kept. Imported services carry `origin: adguard`, `license: GPL-3.0` and are marked in the UI; on an id both have, kudurru's entry wins (the import is counted as shadowed), so turning the opt-in on never changes what an existing policy blocks. AdGuard groups map to kudurru categories (`messenger`→`messaging`, `social_network`→`social`, `streaming`→`video`, unknown→`other`) | The project owner's choice to offer AdGuard's broader list without distributing GPL data | Side-by-side prefixed ids (duplicate tiles; an id's meaning would depend on the opt-in); bundling the data |
| D148 | Deleting a zone | `DELETE /zones/{zone}` deletes the DNSZone and every DNSRecordSet whose `zoneRef` names it, in any namespace: one transaction on the sqlite driver (`store.MultiDeleter`; one audit row per object), record sets first and the zone last on the kube driver, so a failure keeps the zone (the error says how many record sets went) and a retry finishes. It needs `zones.delete` on the zone, plus `records.delete` there when record sets exist; it refuses with 409 `managed_by` (all offenders in `error.objects`) when the zone or a record set is owned by a file, an integration, Kubernetes or the system, and with 409 `default_zone` for the root zone `.`, which routes every query outside the other zones (its upstream group and query access apply to them): it is edited, not deleted. `?dryRun=true` runs every check and returns the same summary (record sets, record set and endpoint counts) without deleting. The API adds one `zones.delete` audit event and flushes this node's cached answers at or below the apex. The generic `DELETE /objects/dnszones/{name}` and `dnszones:batchDelete` never cascade: a zone that record sets reference is 409 `zone_not_empty` (listed in `error.objects`), the root zone 409 `default_zone`. `GET /zones` reports `deleteRefusal` per zone | Record sets reference zones by name without ownerReferences, so deleting only the DNSZone orphaned them (`ZoneNotFound`); one cascading route keeps the generic delete Kubernetes-like and the two paths from disagreeing | Cascading on the generic route (a surprising side effect of `kudurru delete`/`batchDelete`); ownerReferences on record sets (only the Kubernetes garbage collector honours them, and only after the fact; the sqlite driver has none, and every writer, integrations and kubectl users included, would have to set them); allowing the root zone to be deleted (it silently changes routing and drops its query ACL) |
| D149 | Attach for policies the dashboard may not edit | The Allowlist and Denylist show, for each file-, Kubernetes- or integration-managed policy that lacks a ui list, the exact `spec.allowListRefs`/`spec.blockListRefs` YAML merged with the policy's current refs, and say where the definition lives from `kudurru.io/managed-by` (objects-directory file: watched, no restart; `objects:` in the config file: restart; Kubernetes: manifest or a `kubectl patch` merge patch; integration: change it at its source). `?add=<domain>` on `/allowlist`, `/denylist` and `/domains` opens the Add dialog prefilled (the query log links there) | One writer per object (D9) stays intact while the dashboard still tells the admin exactly what to change | The API editing file-managed objects; a `/allowlist/new` route |
| D150 | Own-token routes at any held scope | `/auth/tokens*` are guarded by `tokens.own` at cluster scope, then at the scope of each effective grant and of each scope of a scoped API token, each checked by the RBAC engine so token scopes still restrict; `?all=true` and other owners' tokens still need `tokens.read`/`tokens.delete` at cluster scope. Token `lastUsedIP` comes from the API's client IP (trusted proxies) | Zone-admins could not mint tokens; the proxy address was recorded instead of the client | A cluster-scope `tokens.own` grant for every role |
| D151 | Host-network images and edge privileges | `<version>-hostnetwork` is the ko binary with `cap_net_bind_service=+ep` (ko cannot set file capabilities), built and signed by publish.yml from the verified ko digest; the chart's `hostNetwork.enabled` runs it. The main image stays without the capability (a `+ep` binary refuses to start where the bounding set lacks it). For eBPF and host-network dnsdist the container starts as root with only NET_BIND_SERVICE, SETUID, SETGID, KILL (+ BPF, SYS_ADMIN for eBPF) and dnsdist switches to 953 (`kudurru edge run --dnsdist-user`), because Kubernetes makes added capabilities effective only for root and file capabilities do not pass through tini under no_new_privs. Supervisor images copy `/ko-app/kudurru` (ko's path) | Port 53 on the host network and eBPF maps could not work as uid 953/65532; the first release would have shipped broken supervisor images | File capability on the main image; running dnsdist as root throughout |
| D152 | Precompressed dashboard | The dashboard is embedded gzip-only (`dashboard/scripts/compress.ts`, files ≥ 1 KiB except index.html) and served with `Content-Encoding`, `Vary` and an ETag per encoding; clients without gzip get it decompressed once; brotli is opt-in (`KUDURRU_WEB_BROTLI=1`, ~0.7 MB more). −1.84 MB binary | D75 headroom; nearly every client accepts gzip | Both encodings embedded |
| D153 | v0.1 security round decisions | Unblock request names are `ur-` + 16 hex of HMAC-SHA256 over client and name with an HKDF key from the encryption key (a viewer cannot recover the requester's address); the AdGuard import keeps only single-site `\|\|name^` rules below a public suffix, at most 1,000 services; the guarded list/catalog client checks every request URL, redirects included, for local addresses (also behind a proxy); each node runs at most 4 concurrent cached-body parses (then 503 with Retry-After); releases are published only from the `release`/`release-candidate` branches, check out `refs/tags/<tag>` without persisted credentials, build supervisor and host-network images from verified digests, and every `cosign verify` pins `publish.yml@refs/heads/release(-candidate)` (dev-docs/milestones/m5/security-v0.1-round.md) | Findings V1–V17 | Accepting the risks |
| D154 | Writable directories in every image | The ko release images build on `ghcr.io/robertherbaugh/kudurru-base` (images/base: the pinned distroless image plus `/var/lib/kudurru` 0755 and `/run/kudurru` 1777, both owned by 65532), which publish.yml builds, signs and passes to goreleaser by digest (`KUDURRU_BASE_IMAGE`, `KUDURRU_DEBUG_BASE_IMAGE`); the Dockerfile and images/hostnetwork create the same directories; `make volume-check` and publish.yml start each image on fresh named volumes | A fresh Docker named volume takes the owner of the image's directory; distroless has none, so `-v kudurru-data:/var/lib/kudurru` (docs/deploy/docker.md, every compose file) came up root-owned and kudurru exited with `open state.db: permission denied`. ko cannot add directories or owners | Building the images from the Dockerfile instead of ko (larger pipeline change before the first tag); documenting a `chown` init container (every user hits it first) |
| D155 | Audit routes when nothing is stored | `GET /audit` and `/audit/export` take an explicit `AuditReader`; without one, the storing member of the recorder chain answers (`audit.ReaderOf`), and a chain that stores nothing (the kube driver's `"log":"audit"` lines only) answers `503 unavailable` before the export stream starts; `audit.ErrNotStored` maps to 503 everywhere | In mode=kubernetes the Audit tab was a 500: the recorder chain always type-asserted as a reader and then failed (#10); the export answered 200 with an error line | Type-asserting `Options.Audit` (an `audit.Multi` always satisfies `audit.Reader`) |
| D156 | Audit in the kube driver | The kube driver keeps no audit rows, so `serve` wraps the store in `audit.WrapStore` before the first writer takes it: every successful Apply and Delete is recorded as `<kind>.create|update|delete` with the actor of the request (`store.WithActor`), the manager and the before and after documents (without `managedFields`; internal kinds without documents), and an apply that leaves the resourceVersion unchanged records nothing. Status writes are not recorded (controllers and token use write them; the apiserver audit log covers them). Events go to the `"log":"audit"` lines first and then to the node's audit spool `audit-spool.db`: a hash-chained SQLite table (`audit.ChainHash`, microsecond timestamps, documents hashed byte for byte), capped at 50 000 events (oldest dropped, `kudurru_audit_spool_dropped_total`), pruned after 400 days by every node, with a `source_id` UUID so a later shipper keys events by (source, id). `GET /audit` reads the spool of the replica that answers. A recorder failure is logged and never fails the write, which the apiserver has already accepted | In mode=kubernetes no node stored audit events and the Audit tab could never work (#10); a write through the API left no kudurru record of who made it. The spool is the durable local copy that a cluster-wide audit store (the AuditLog CRD, Postgres) is filled from | Recording in the API handlers (misses bootstrap, file objects and controllers, and duplicates the sqlite driver's rows); failing the write when the recorder fails (the object is already stored; config management must not depend on the audit store); recording status writes (one event per token use and controller pass) |
| D157 | AuditLog batches | With the kube driver and no central database (`audit.store: auto` → `auditlog`), every node ships its audit spool (D156) into `AuditLog` objects (`audit.kudurru.io/v1alpha1`, a separate group so the store, its validator, backups and `kubectl get kudurru.io` never see them; CRD in `deploy/kustomize/crd/audit`, shipped by `kudurru-crds`) in the release namespace, and every replica reads all of them: `GET /audit` is the same on every replica and survives pod replacement (#10). A batch is immutable (the chart grants `get, list, watch, create, delete` and no update), holds at most 1000 events and 512 KiB, and is named `<node>-<source>-<first id>` so a retried create finds it; `audit.Shipper` batches (5 s after the first waiting event, at least 30 s between batches of one node unless 500 wait, and once more at shutdown), retries with backoff and reads its cursor back from the batches after a failure, so delivery is at least once and in order and the spool keeps events while the apiserver is away. Batches carry the spool's hash chain and an HMAC-SHA256 seal keyed by HKDF from the cluster encryption key (`kudurru/audit-seal/v1`); `auditlog.Verify` checks events, links between a node's batches and seals. Beyond 2000 denied or failed events per node per hour such events are stored as stubs (no documents, reason or request; only their link verifies) so an audited request flood cannot fill etcd; successful actions are always stored in full, so a flood of denials cannot blank a later change, and a retried batch is charged once. The reader lists batch metadata (annotations: time range, kinds, actors), reads only the batches a page needs (cached; they never change), orders events by (time, source, id) with an opaque cursor (`audit.Query.Cursor`, `Event.Cursor`, `Event.UID`) and adds the serving node's unshipped events. The leader deletes batches older than `audit.retention` (hostOnly, 400 days by default). A missing CRD falls back to the node's spool with a warning | In Kubernetes the audit log must be the same on every replica behind a load balancer without sticky sessions and must survive pod replacement (the chart keeps `/var/lib/kudurru` on an emptyDir and allows per-pod volumes only with one replica); the apiserver is the replicated store every Kubernetes install already has. Audit volume is low and batched, capped and pruned, which keeps this clear of the in-etcd time series Gravity regretted (observability §5) | One object per event (etcd write per action); mutable hourly objects (needs update RBAC, so history could be rewritten); Kubernetes Events (1 h TTL); per-pod spools with scatter-gather (lost with the emptyDir, partial when a pod is down); requiring a database for multi-replica installs |
| D158 | Optional central database | `database.dsn` (hostOnly, writeOnly) names an optional PostgreSQL 16+ database (17 preferred) (pgx v5, pure Go) for runtime data of every node: audit events, query log and statistics. Configuration never goes there (CRDs, `state.db`). `internal/central` opens the pool without dialing; an always-on runnable connects, checks the server version and migrates under `pg_advisory_xact_lock`, then pings; the database is never part of readiness and never on the DNS path. The schema carries `schema_version` and `compat_version` in `kudurru_meta`: a node migrates an older schema, uses a newer one while `compat_version` ≤ its own schema, and otherwise stops using the database (`CentralIncompatible`) instead of refusing to start, unlike the state.db rule of §4.3, because rolling upgrades always mix versions and the data is optional. `source` and `ship_cursor` key everything shipped by (source UUID, local id) with the cursor written in the same transaction as the rows. A leader-only `central.Janitor` runs retention jobs. `/info/messages` reports `CentralUnreachable` after a minute and `CentralIncompatible`; `kudurru doctor` checks the database. CI runs the tests against PostgreSQL 17 in the test job and 16 and 18 in a matrix (`KUDURRU_REQUIRE_POSTGRES` turns a skip into a failure) | Per-node SQLite with scatter-gather (principle 5) leaves data missing when a node is down or replaced and gives each replica behind a load balancer a different answer; long-term history needs one durable place. PostgreSQL is what operators already run (CloudNativePG, managed services), pure-Go drivers exist, and plain declarative partitioning covers query-log volume without extensions | ClickHouse as the only central store (no audit, heavier to run); a required database (breaks the one-container and Raspberry Pi shapes); configuration in the database (the apiserver and state.db already replicate it); refusing to start on schema skew; testcontainers-go (Docker client dependencies in go.mod) |
| D159 | Audit in the central database | `audit.store: database` (what `auto` picks when `database.dsn` is set) ships every node's own audit store, the spool of the kube driver or the `state.db` table of the sqlite driver (`audit.StateSource`, named by a `state.db` meta `audit_source_id`), into one `audit` table with `audit.Shipper` (1 s batching), and every replica reads it (`pgaudit.Reader`); the local stores keep their own chains. Each append locks the one-row `audit_chain` head (`SELECT … FOR UPDATE`), skips what the source's cursor already covers, gives every event the next id and `audit.ChainHash` over the previous head, inserts with COPY and moves the head and the cursor in the same transaction: one chain across all nodes, gapless commit-ordered ids, so the API's plain id cursor works. Hashed fields are stored exactly (documents as `text`); the node's own chain is kept in `origin_hash`. `pgaudit.Verify` walks the chain from its pruned anchor to the head and catches changed, removed and truncated events. Retention (`audit.retention`, leader janitor) deletes the longest prefix of the chain older than the cutoff, so node clock skew never leaves a hole, and records `pruned_through`/`pruned_hash`. While the database is away, events wait in the node's store and `GET /audit` answers 503. Client-controlled text (a `%00` path, any bytes in a User-Agent) is sanitized to valid UTF-8 without NUL before an event is hashed (`audit.Sanitize`: in the API, the spool, the store decorator and again in `pgaudit`), so no event can be refused by a text column and hold its source's later events back; a batch the database still refuses for its data (SQLSTATE class 22) is stored once more without free text. `Verify` reads one repeatable-read snapshot, and housekeeping and purges lift the pool's 15 s read timeout (`SET LOCAL statement_timeout = 0`); purges truncate whole day partitions | Docker and compose nodes have no AuditLog CRD and kept separate audit logs; operators with a database want one chain, one place to query, and history that outlives volumes | Per-node chains only (no cluster-wide order); `bigserial` ids (commit order differs from id order, so a keyset page can skip an event committed later with a lower id); `jsonb` documents (reorders keys, breaks the hash); falling back to the node's own events while the database is away (a cursor of one store is meaningless in the other) |
| D160 | Query log and statistics in the central database | With `database.dsn` and `database.querylog.ship` (default on), every node copies its `querylog.db` into tables that mirror it (§11.2): the same names and integer timestamps, central dictionary ids, a `node` on every rollup row, so a cluster total is a sum over nodes and one reader serves both (D161). The node writes `querylog.db` first; `pgquerylog.Shipper` (an always-on runnable) copies fact rows by local id in batches of 5000 with `COPY`, the source's cursor moving in the same transaction, and every 30 s the rollups: minutes again from ten minutes before the cursor (a late event can still add to a flushed minute) with replace-upserts, which are idempotent because the node's row is the whole truth for its (ts, node, key), and the hours and days the node has completed, whose progress is the stream cursor. Dictionary values travel as names and are mapped onto central ids (`INSERT … ON CONFLICT`, cached; `dict_client` is `UNIQUE NULLS NOT DISTINCT (ip, name)`, PostgreSQL 15, below the supported 16). The fact table is partitioned by UTC day (created ahead by the leader and on demand under an advisory lock) and expired days are dropped whole (`database.querylog.retention.raw`, 7 days); rollups expire as on the nodes. `database.querylog.facts: false` sends only the statistics. Age-based retention on a node keeps rows the shipper has not delivered (`SetShippedThrough`); the size cap still wins | Node-local query logs answer "All nodes" from one replica (#9) and lose a replaced pod's history; a cluster-wide query log and statistics need one place every replica reads | A `querylog.Sink` (in-memory queues drop events while the database is away); keying central rows by (node, id) (ids restart with a new volume); recomputing statistics centrally from facts (privacy modes and short raw retention keep rollups without facts; group-bys over tens of millions of rows); denormalized text columns (rollup keys must compare across nodes); `timestamptz` columns (would need a second reader) |
| D161 | Cluster-wide reads from the central database | With a central database, `scope=cluster` of the stats and query-log routes (and the policy page's traffic and the client counts) is answered by `sqlitelog.NewCentralReader` instead of the scatter-gather over the replicas: the node reader's own code over the mirrored tables (D160) through a small dialect layer (`?` and `?N` bound as `$N`, id lists as `= ANY`, bytewise `COLLATE "C"` for suggestions, and portable SQL in both: subquery aliases, aggregates spelled out in HAVING and ORDER BY, CASE instead of summed booleans, the row's node read from `dict_node`), so both readers give the same answers by construction; a conformance test compares every reader method on a node's file and on the database after shipping. Rollup tiers are complete up to the slowest node active in the last two hours (its shipped hour and day progress); newer data comes from the fact table, as on a node. The response is `scope: cluster`, `source: central`, with every active node and `partial` when a node's statistics are more than five minutes behind. The query log pages on central ids (rows of different nodes in arrival order, within the shipping delay of a second or two); counts stop at 100 001 (`records_capped`); a query action on such a row passes `scope=cluster`. `scope=node` (this node or a chosen one, read through its internal listener) and the live tail stay on the replicas. The central view can follow all active nodes by multiplexing their SSE streams: while live tail is enabled the dashboard shows only incoming rows with node IDs, never merging them with central history; stopping refreshes history with the database's IDs. `DELETE /querylog` purges this node and the database (`central_deleted`), not other nodes' files. A database that is away answers 503 | Scatter-gather has to reach every replica: a replica that is down makes the answer partial and a replaced one takes its history with it; the database holds every node's rows in one place. A second reader written for PostgreSQL (some 1 500 lines that would drift from the node's) | Merging the scatter-gather and the database (double counting while a node's rows are in both); merging the live tail into the central log (its ids are the nodes') |
| D162 | Source-available license, decided before the first public release | The initial source-available grant used the PolyForm Noncommercial License 1.0.0 (superseded for new copies by D166): personal use is free, use by a business needs a commercial agreement. It replaces Apache-2.0 before the first release: the repository was private and nothing had been published, so every tag, release, image and chart carries the new license. The earlier commits stay in the history under Apache-2.0 (the history was not rewritten, because a rewrite cannot withdraw copies that exist anyway); only a snapshot taken before this change can be used under it. The copyright holder is the sole author, so no one else's consent was needed. A copyleft license cannot say "a business needs an agreement", because no OSI license may restrict who uses the software; contributions need the DCO plus a separately accepted contributor agreement (`CONTRIBUTING.md`) so commercial licenses stay possible. The rule against porting GPL, EUPL or other copyleft code is stricter now, since such code cannot sit in a non-copyleft work | The project owner on 2026-10-12: protect the product from the start, keep it free for homelabbers and self-hosters, and require an agreement from businesses, including for the parts that are not licensed | Apache-2.0 core with a proprietary `ee/` (businesses use the core free); AGPL-3.0 core with dual licensing; PolyForm Small Business or BSL (kept as the fallback if a free band for small companies is wanted) |
| D163 | License keys and the `ee/` features | Features that need a key live under `ee/` (`ee/oidc`, `ee/rbac`, `ee/auditstore`, `ee/longterm`, `ee/edge`, `ee/integration`) with extra terms in `ee/LICENSE.md`; two more are rules in `internal/license` (unblock requests, and a limit of one policy without a key, the seeded default); a feature is split along the line of what a node runs without a key, so the permissions table, the engine, the built-in administrator, the audit event types and the log recorder stay in `internal/`; a free personal key (3 nodes, non-commercial) and business keys unlock them and more nodes (`internal/license`, [licensing.md](../docs/architecture/licensing.md)). A key is an Ed25519-signed JWS verified offline against public keys embedded in the binary, read from `--license-key`, `KUDURRU_LICENSE_KEY`, `KUDURRU_LICENSE_KEY_FILE` or `<dataDir>/license.key` and reread every minute. **Licensing never touches the DNS data path:** without a valid key the node runs as one node without the `ee/` features, an expired key works for a 30-day grace period, API configuration writes answer `402 license_required` only while the cluster is larger than the key allows, roles beyond the built-in administrator and OIDC sign-in are switched off, the audit trail, statistics older than 24 hours and unblock requests answer 402, a second Policy cannot be created through the API (the API counts every Policy in the store, so the bootstrap's `Policy default` fills the limit of one; existing policies are never removed), and the webhook is not started (the built-in administrator, tokens and DNS are unaffected), and the edge supervisors check once at start and refuse only a start with no key that ever covered the edge (a key in its grace period, or expired since, starts them with a warning, so a lapse never becomes a crash loop in the data path). Metrics `kudurru_license_*` and `GET /api/v1/license` expose the state. The model follows Portainer Business (key by flag, environment variable or file; free small-node keys) and Pangolin (a free enterprise key for personal use). Reading the audit trail is gated at the API (`GET /audit`, `/audit/export`, whatever stores it: `state.db`, the AuditLog objects or the central database of D155 to D161); storing, spooling and shipping events are not. DNS NOTIFY (`internal/notifier`), the block page and everything on the query path stay free | The project owner on 2026-10-12: gate new and existing features (then RBAC, the audit trail, long-term views, unblock requests and a two-policy limit), a free key with a limit of 3 nodes for homelabbers. The project owner on 2026-10-13: without a key only the seeded default policy, and the API creates no other name (a deleted default can be made again) | Online activation against a license server (a DNS server must not depend on one); gating the data path or removing nodes when a key lapses |
| D164 | The blocked-services catalog stays Apache-2.0 | `internal/listdata/catalogue` (data written for kudurru, D60, D120) keeps its own `LICENSE` (Apache-2.0) and provenance file, because it is meant to be published in a separate data repository and fetched at runtime like any list; the code that loads it is under the repository license. The third-party icons keep the Apache-2.0 license of their source | D60 called for a permissively licensed, reusable catalog | Relicensing the data with the code |
| D165 | License portal for issuing keys | `license-server/` (Bun 1.4.3, TypeScript, Postgres; one container, uid 65532) issues keys in the `internal/license` format (Ed25519 compact JWS, byte-identical to `license.Sign`, checked against Go's golden vectors and the node's own code); sign-in with OpenID Connect only; roles `customer`, `license_admin` and `superadmin`; the UI uses the dashboard's Astryx design system and Kudurru theme. Customers reach a customer's licenses through one-time invite links; the kudurru.io form (after an email confirmation) and the portal request free personal keys, with the non-commercial certification recorded. A personal key, new or renewed, is signed only after a license admin approves it: each request mails the admins (`ADMIN_NOTIFY_EMAIL`, else every license admin and superadmin with a verified email) the details and a one-click approve link (a token per recipient that decides once within 7 days, only its SHA-256 stored; opening it changes nothing, approving is a POST from the page), and the key goes to the requester by mail after approval; blocked addresses and withdrawn histories get no link. `PERSONAL_AUTO_APPROVE=true` signs at once, as an opt-in. The signing key comes from a secret at run time, never from the database or the image. Least-privilege database roles (owner for migrations, runtime role with per-table grants). Hosted on Cloudflare Containers behind a Worker (`license-server/cloudflare`). No node ever contacts it, so D163 stands. Business requests from the form and deleting users: D181; Order Forms: D183 | The project owner on 2026-10-12: a license server that generates the keys, as a Docker container with Bun, TypeScript and Postgres, OIDC with customer, license admin and superadmin roles, the Astryx design elements; personal requests from the website get a key, business requests stay email; hosting on Cloudflare Containers with PlanetScale and Cloudflare Email Service. The project owner on 2026-10-13: require approval for the license instead of cutting it automatically, with an email that has the details and a one-click URL for admins to approve, then the key is cut and mailed | Online activation (D163); signing with `hack/licensegen` by hand only; a hosted licensing service; linking customers by IdP email alone; automatic issue of personal keys as the default (with a daily cap; it stays an opt-in); an approve link that needs a sign-in (one click from the mail was asked for) |
| D166 | Core community grant and folder-specific licenses | Core DNS and dashboard use the Kudurru Community License (`LICENSE`); `ee/LICENSE.md` governs enterprise entitlements and `docs-site/LICENSE.md` governs docs previews, contributions and commercial private mirrors. Anyone may consult public docs. `website/` and `license-server/` are excluded from community and commercial grants and available for inspection only under their source-viewing notices. The website credits kudurru project maintainers, defined in `website/LICENSE.md` as its applicable copyright holders; this collective attribution does not transfer ownership, and permission requests go to hello@kudurru.io. The license server is copyright Robert Herbaugh. Business and government production requires an accepted `COMMERCIAL_LICENSE_AGREEMENT.md` and Order Form; Commercial Core does not automatically authorize enterprise modules. Community contributions require separate contributor agreement acceptance plus DCO sign-off. Keys and grace periods grant no additional legal rights. Historical licenses and third-party terms remain in effect | Project owner on 2026-10-13: apply supplied community, commercial and folder licenses; remove drafting/legal-review notes | One repository-wide license for website and issuance source; implied commercial rights from a key |
| D167 | Separate VIP-link tracking and address uniqueness | With `--vrrp-interface` (`vip.keepalived.vrrpInterface`), track each separate VIP interface so either link failure faults the instance. Reject duplicate VIP addresses, including differing prefix lengths, in the renderer and the chart's `vip.keepalived.vips` list. Single-interface and single-VIP behavior stays the same | An unnumbered DMZ interface cannot supply the node InternalIP used by Kubernetes peer discovery, and its link failure must still withdraw its VIPs. Independent instances must not manage the same address | Tracking only the VRRP link; allowing multiple instances to own the same address |
| D168 | Supervised dnsdist health compatibility | Decode numeric backend IDs from dnsdist's HTTP API and read `DNSResponse.rcode` in Lua validators. Accept LOCAL headers from listed peers without an identity-delegation TLV and keep the transport peer, as for plain DNS. PROXY commands retain the shared-secret check | The real dnsdist 2.1.2 API uses numeric backend IDs. Its health probes carry LOCAL headers without query-rule TLVs. The Lua response has no `dh` property, so the validator threw an exception after the probe reached the server | Disabling readiness or forcing backends up; accepting unauthenticated PROXY identity delegation |
| D169 | Native API migrations | Tools wizard and `import pihole\|technitium` share a live API converter. Preview returns objects, a settings patch and warnings; direct import uses normal validation, ownership, permissions and licensing, with per-object results and settings applied only after all objects succeed. Source credentials are transient; Pi-hole uses a temporary app-password session, Technitium a bearer token. REST access requires settings.write; redirects are refused. Unsupported semantics are reported | Move supported configuration without manually exporting files or rebuilding objects; validate against disposable source containers | Requiring manual manifests or native backup-format reverse engineering; persisting source credentials; silently broadening unsupported rules |
| D170 | Built-in validating recursive resolver (v0.1) | An upstream group takes `mode: forward` (default, `servers` required) or `mode: recursive` (no servers; `strategy` ignored; `timeout` bounds the whole lookup, default 4 s). Recursive groups share one resolver, `internal/recursor`, behind dnsproxy's upstream interface, so routing (Policy, forward zone, conditional rule), health, metrics, the query log and the response cache treat it as the server `<group>/recursive`. It resolves from built-in IANA root hints with priming, minimizes QNAMEs (RFC 9156, relaxed), never sends ECS, answers RFC 6303 and special-use names (`home.arpa`, `local`, `internal`, …) itself, caps the work per client query (NXNS, KeyTrap CVE-2023-50387, NSEC3 iterations RFC 9276) and validates DNSSEC from built-in root trust anchors (KSK-2017, KSK-2024): bogus → SERVFAIL with EDE 6-12 and status `dnssec_bogus` unless the client set CD; AD for DO/AD clients; the state fills the query log's `dnssec` column. Settings `upstreams.recursion.{dnssec,qnameMinimization,ipv6,trustAnchors,rootHints}`, cluster-scoped and hot-reloaded. Forwarding to 1.1.1.1/9.9.9.9 stays the default for new installs. Deferred (dev-docs/followups.md): aggressive NSEC caching (RFC 8198), RFC 5011 trust-anchor rollover, negative trust anchors, validation of forwarded answers, DNS cookies, a local root zone (RFC 8806) | Project owner on 2026-10-16: ship recursion with validation in v0.1.0, release date unchanged. Privacy-minded home users otherwise run Unbound or Technitium beside kudurru only to avoid sending every name to one provider; as a group mode it reuses the routing, health, cache and query-log paths instead of adding a pipeline stage. Forwarding stays the default because recursion needs outbound UDP/TCP 53 to any host, which many networks and ISPs block or intercept, and cold lookups take several round trips | Keeping forwarding only (users keep a second resolver); recursion without validation; a separate `dnssec` stage validating every upstream (the v0.2 plan, still open for forwarded answers); making recursion the default; embedding Unbound (cgo, a second process) |
| D171 | DHCP server support (supersedes D42/D52) | Add DHCP server support to the roadmap; D172–D178 define the implementation. **Release target superseded by D179:** opt-in DHCPv4 ships in v0.1.0 | Project owner on 2026-10-15: include DHCP in the supported feature plans; release target revised on 2026-10-16 (D179) | Keeping DHCP out of scope through v1.0 with only a possible v2.0 target |
| D172 | Initial DHCPv4 implementation | Start with an opt-in server for one directly attached Linux LAN and the SQLite driver. Node-scoped, host-only, restart-required settings; a separate exclusively owned `dhcp.db` records offers, bindings and decline quarantine with WAL/FULL commits before replies. MAC reservations, renewals, release and INFORM are supported. No DHCPv6, relays, failover, import, automatic DNS records or DHCP API/UI in this slice. Details: [architecture](../docs/architecture/dhcp.md), [deployment](../docs/deploy/dhcp.md) | Project owner selected DHCPv4 first with persistent leases and reservations. Lease allocation needs durable exclusive ownership; independent DNS replicas cannot safely allocate from the same pool | Enabling DHCP on every Kubernetes replica; in-memory leases; advertising IPv6 or failover before implementation |
| D173 | Complete standalone DHCPv4 | Extend D172 with named scopes, one listener per interface, exact trusted-relay mapping, durable API-managed options/reservations with revision checks, atomic native/dnsmasq lease merge, and immutable lease DNS/policy snapshots. Dedicated DHCP domains answer only without a matching zone or beneath the catch-all forward root; specific zones and primary root retain precedence. File mode remains the default; topology stays host-configured. Validate with stock dhcpcd in disconnected namespaces. DHCPv6 and failover stay deferred in this initial slice (failover superseded by D176) | Project owner selected finishing DHCPv4 first. API edits must preserve live ownership and serialize with replies; DNS queries must avoid SQLite and configured-zone mutation | Replicated independent allocators; implicit configuration ownership; lossy identity/quarantine export; DNS record overlays in controller-managed zones |
| D174 | Dashboard-owned DHCP setup and staged apply | Extend D173 with complete node-local UI setup, including enabled state and topology, in atomically persisted `dhcp-settings.json`. Fresh setup needs no DHCP YAML; existing host setup requires explicit adoption and all DHCP environment overrides must be removed. Saves require a setup ETag and observed per-scope DB revisions, validate complete scopes even when disabled, and stage the next restart without opening listeners or mutating live stores. Read views preserve pending desired edits and overlay current effective options elsewhere. Lease schema v3 atomically applies a scope token, expected revision and live-state validation once; matching tokens preserve later live API edits. Renamed/removed scope DBs remain intact and retirement can block startup. UI-owned DHCP activation failures leave DNS/API available for repair; corrupt setup, environment conflicts and file-owned startup failures still fail closed. Details: [architecture](../docs/architecture/dhcp.md), [setup and recovery](../docs/deploy/dhcp.md) | Project owner requested full DHCP configuration through the UI. Topology needs an explicit restart boundary; saved defaults must not overwrite live settings, and concurrent edits or startup retries must preserve lease ownership | Rewriting operator YAML; automatic live socket rebinding; replaying stale option seeds on restart; discarding retired lease databases; making the repair UI unavailable after a recoverable DHCP activation failure |
| D175 | Isolate DHCP packet and storage failures after startup | After successful startup, lease-handler or storage failures log at ERROR and drop only the affected packet; no successful lease reply is sent for a failed operation. Later requests and retries continue in both file- and UI-owned scopes. Existing per-packet network timeout/ICMP handling remains; unexpected socket failures remain fatal. Startup ownership rules are unchanged, and setup `runtimeError` remains startup-only. Details: [architecture](../docs/architecture/dhcp.md#packet-failure-isolation), [troubleshooting](../docs/deploy/dhcp-management.md#diagnose-packet-failures-after-startup) | Project owner approved runtime isolation on 2026-10-16 after [PR 66 review](https://github.com/robertherbaugh/kudurru/pull/66#discussion_r4290059323): optional DHCP storage failure must not stop core DNS | Terminating the node for a lease-handler/storage failure; sending uncommitted OFFER/ACK replies; swallowing unexpected socket failures |
| D176 | Three-voter DHCPv4 quorum failover | Exactly three static voters use HashiCorp Raft, durable log/snapshots and mutually authenticated peer transport. Only a current majority-authorized leader serves DHCP; lease and typed management operations commit before success. Followers forward management and wait for local SQLite application. The sole explicit bootstrap peer may adopt standalone state; foreign joins and missing initialized Raft history fail closed. Preserve local interface/server IP on restore and reserve all three server addresses. Established shared topology/voters are fixed; live options/reservations/import replicate, local setup cannot replay stale seeds. DHCPv6 remains excluded. Details: [architecture](../docs/architecture/dhcp.md#three-node-quorum-failover), [deployment and recovery](../docs/deploy/dhcp-failover.md) | Project owner requested completing DHCPv4 including failover on 2026-10-16 and selected three Kudurru nodes with built-in quorum. Majority ownership prevents partitioned independent allocation; TLS binds peer identity; T2 reaches a replacement leader without a VIP | Two independent allocators, majority-free fallback, VIP-only ownership, silent rebootstrap or local setup overwriting committed cluster settings |
| D177 | Durable DHCP DNS name ownership and indexed lease transactions | Earliest live bound claimant owns an unreserved name across scopes; reservation names take precedence even offline. Persist claim rank through renewal/restart, end it on release/expiry/name or domain change, and issue fresh import tickets so old backups cannot steal ownership. A private directory-wide sequence/gate orders standalone publication; Raft epoch/index/ordinal orders replicas. Schema v4 stores claims and replica progress. Address/client indexes and incremental immutable DNS shards replace full reads/rebuilds on each renewal, retaining FULL-synchronous SQLite and commit-before-publication. Benchmarks at 1k/10k precede optimization and document limits | Project owner requested completing deferred DHCPv4 ownership and scale work; [PR 66 review](https://github.com/robertherbaugh/kudurru/pull/66) identified duplicate-name denial of service and repeated full-state scans. Names/MACs remain unauthenticated | Letting a later duplicate suppress an established name; importing old claims over live owners; query-time database reads; weakening durability for performance |
| D178 | Complete DHCPv4 conflict and network-option coverage | Pre-offer bounded ARP on direct links or explicit routed ICMP; durable owned-offer quarantine and at most three candidates, with errors failing closed and an explicit disabled mode. Exact authorized RFC 3527/5107/8357 relay extensions; NTP, search, classless routes, architecture boot profiles and advisory client FQDN. Replies advertise the committed allocation grant despite concurrent settings changes. Disconnected stock-client CI verifies wire behavior and both capability profiles | Project owner requested previously deferred DHCPv4 work; silent static address conflicts and ordinary relay/boot deployments need explicit protocol support | Assuming probe errors mean vacant; wildcard relay trust; uncommitted expiry in ACK; serving boot files or claiming external DNS UPDATE support |
| D179 | DHCPv4 release scope (revises D171) | Include the opt-in Linux/SQLite DHCPv4 implementation from D172–D178 in v0.1.0, including dashboard setup and three-node quorum failover. Move the changelog entries into v0.1.0 and align site, docs and www references. DHCPv6 remains excluded; deployment acceptance remains operator-specific | Project owner on 2026-10-16: include DHCPv4 in all v0.1.0 references across site, docs and www | Deferring implemented DHCPv4 to v0.2.0; claiming DHCPv6 or kube-store support |
| D180 | DHCPv4 on the Kubernetes store (revises D172, D176, D179) | With `store.driver: kube`, scopes are cluster-scoped `DHCPScope` objects (kudurru.io, one writer: `api` or a Git owner such as `gitops:argocd`) and leases are `DHCPLease` objects in their own runtime group `dhcp.kudurru.io`, named `<scope>.<address>`, outside the store, backups and zonecache. The holder of Lease `kudurru-dhcp` serves through a relay to a LoadBalancer address (`dhcp.serverIP`, option 54), labeling its pod `kudurru.io/dhcp-active` for the Service selector; a ValidatingAdmissionPolicy limits the ServiceAccount's pod updates to that label on its release's pods. Each lease decision commits its exact row changes to the apiserver (create, or update and delete under resourceVersion) before the local SQLite working copy commits; every replica rebuilds that copy from the objects and follows a watch, and the serving replica deletes expired objects. Dashboard edits are validated against live leases; Git edits apply and report conflicting leases in status. The SQLite path is unchanged | Project owner on 2026-10-17: the main cluster uses the kube store, DHCP must be configurable from whichever replica a sticky session lands on, and scopes may come from Git or the dashboard. Object names make the apiserver refuse a second owner of an address even during an election overlap, which a leader check alone cannot. Lease writes are one object per change, about one per bind or renewal, far below the AuditLog budgets (D157) | Raft voters inside pods (needs persistent per-pod volumes and static identities); one object per scope holding all leases (1.5 MiB limit, write contention); forwarding the node-local API between replicas (keeps state on one pod; lost with an emptyDir); hostNetwork DaemonSet on client VLANs (more privileges; the relay path needs none) |
| D181 | License portal takes business requests and deletes users | The kudurru.io form posts a business request (name, email, company, edition `core` or `enterprise`, nodes, details) to `POST /api/v1/public/business-requests` on the license portal (`license-server/`, D165): stored at once in `business_requests` (no confirmation link, since nothing is signed until a license admin issues a license), with the form's honeypot and per-network and per-email limits; the requester gets a "we have your request" mail and the approvers (`ADMIN_NOTIFY_EMAIL`, else the license admins and superadmins) a mail that points to the Requests page. There, the Business tab lists them; a license admin issues a license from one (the issue wizard filled in: a new organization customer, the contact email, Commercial Core as plan `business` or Enterprise as plan `enterprise`, the nodes), which marks it issued with the license, or closes it with a note. When the portal cannot take the request, the form opens the email it used to send. A superadmin may also delete a user: their sessions and customer memberships go, other records keep their label, `user.deleted` records issuer, subject and email; nobody deletes themselves or the last superadmin, and signing in again creates a new user without roles. The runtime database role gains `delete` on `users` (migration 0004) | The project owner on 2026-10-16: business requests should show up in the license center rather than only as email, which will also help when billing is added; there was no way to delete user accounts (the switch to a new IdP left an old, unreachable superadmin); a "got it" email to the requester and an email to the admins; a hard delete | Business requests by email only (D165, superseded for the form); a confirmation link before a business request is shown; anonymizing a user instead of deleting the row; a one-click link in the business request mail |
| D182 | Migration history and nameserver cutover | API imports create internal `MigrationRun` journals through the store (SQLite rows or labeled Kubernetes Secrets), excluding credentials and URL paths/query parameters. Optional target DNS hostnames replace apex NS and SOA primary during import; a later reviewed cutover uses the stored zone set, annotation, UID, expected authority, run revision and per-zone versions through normal authorization/ownership/validation. Progress persists after every object write and failures stop further writes. Interrupted attempts can be retried after ten minutes; explicit adoption tracks recognized older API imports without replacing records. Dashboard and CLI expose the same history and actions | The project owner on 2026-10-18: keep a log of migration runs and prepare source decommission without manual per-zone NS changes. The dashboard suggests the navigated hostname, with editable targets because management ingress and DNS service addresses can differ. IP URLs and optional VIP discovery produce in-zone NS hostnames with A/AAAA records; CLI defaults derive from the destination context, never the source. Address cutovers check conflicts and reviewed record-set versions, require records.write and journal address writes before authority changes. Partial writes and reimports require durable identity and concurrency checks | Process-local history; a separate journal database; inserting literal IP addresses in NS records; reimporting records just to change authority; automatically changing unrelated delegations or shutting down source servers |
| D183 | Order Forms in the license portal, accepted electronically | The license portal (`license-server/`, D165) holds Order Forms, Schedule A of COMMERCIAL_LICENSE_AGREEMENT.md, as data (`order_forms`, migration 0006; numbers `OF-<year>-<n>` from a sequence): a license admin drafts one for a customer, or from a business request (D181) for a new organization customer; the fees start from the website's price list (`shared/pricing.ts`, a drift test against `website/src`) and may be changed. Sending freezes the text: the portal renders the document (`shared/orderForms.ts` `orderFormDocument`, Markdown), stores the text and its SHA-256 (every kept copy is the stored text plus the acceptances), makes every authorized contact a member of the customer and mails them (an invite link when they have no access). A signed-in member whose membership address is an authorized contact accepts it (full name, title, "authorized to bind", the hash they read; time, client address and user agent kept) or declines it with a note; the approvers are mailed. A sent or declined form may be revised (a new version, the old hash no longer accepted); a license admin countersigns an accepted form with the same hash (executed, final; the contacts get the signed copy), and one license may be issued from it (POST /licenses with `orderFormId`; the server refuses a key outside the form: another customer or plan, more than its production plus nonproduction nodes, other features, an expiry after the subscription end; its business request becomes issued). Anything but an executed form may be voided with an internal reason. New permissions `self.order` (customer), `orders.read` and `orders.write` (license admin). Runtime grants: select, insert, update on `order_forms`, usage on its sequence | The project owner on 2026-10-18: build the order form into the license server, as a full electronic-acceptance flow (the admin drafts and sends, the customer accepts signed in, the licensor countersigns, a license is issued from it), with the fees prefilled from the pricing | A drafted document signed outside the portal (DocuSign) and recorded by an admin; a customer-filled order request in place of the business request; a typed price list in the portal's configuration; signing with a drawn signature or an external e-signature service |
| D184 | Background migrations with item selection (extends D169, D182) | Dashboard and CLI start reviewed plans through `POST /imports/start` (202 plus run ID/Location), independent of browser cancellation, retaining caller identity and per-object checks. At most two jobs run per API server, bounded to ten minutes. Existing store journals track selected objects/settings as pending, running, success/error or not applied, and unselected items as skipped; selected settings use one patch only after every selected object succeeds. Capacity is checked before writes; journal failures stop further work. The dashboard polls a dedicated report URL with success/error counts and individual details; CLI returns the run immediately or waits with `--wait`. Selection names explicit object identities and top-level settings sections; absent selection means all for compatibility, empty/unknown/duplicate selections are refused. Dependencies are reviewed by the user. Reviewed payloads stay in memory, with no credentials persisted and no replay after restart; expired running reports become interrupted with unconfirmed outcomes. Shutdown cancels and joins workers. Synchronous `/imports/apply` remains compatible | Project owner on 2026-10-18: imports should run in the background with progress and a persistent details page, retain visible successes when some items fail, and allow individual items to be selected | Holding the wizard open for all writes; a single error banner hiding successful imports; always importing every item; automatically replaying writes after a restart; a separate queue database |
| D185 | Local DNS overlays with missing-name fallback | Primary zones opt in with `spec.fallback: {}` (normal policy/conditional/default routing) or `fallback.upstreamGroup` (a forwarding or recursive group). Local records, wildcards and empty non-terminals keep missing-type NODATA; absent names continue after the authoritative stage. Generated apex SOA/NS/ANY remain local, while other apex types fall back until an explicit apex record exists. ACLs and snapshot gates fail closed. Local alias chains may continue through fallback; mixed answers clear AA/AD and never claim whole-chain DNSSEC security. Lookups precede the response cache; fallback configuration joins the content hash so edge serial-based flushing follows routing changes. Transfers and NOTIFY are unavailable for partial zones. Dashboard creation/settings and shared CRD validation expose the option | Project owner requested one `herbaugh.network` zone with local overrides and public resolution for other names ([#95](https://github.com/robertherbaugh/kudurru/issues/95)) | Implicit fallback for existing primary zones; per-host zones as the only option; fallback for missing record types (can expose public IPv6 addresses); copying the entire public zone; transferring partial zones as complete authority |
| D186 | Explicit upstream settings ownership (extends D106; adoption precedent D174) | Kubernetes sections default to GitOps, and only the complete upstreams section is switchable initially. Adoption copies effective values and owner into a separate API-created ConfigMap, using resourceVersion checks; Helm never renders it. UI-owned sections replace file sections, conflicting declarations are logged and exposed, and environment locks remain. Each replica polls shared state through the settings watcher; a node-local last-good copy supports offline startup. Docker retains writable settings files with ownership metadata in the same atomic rename. settings.write authorizes audited adoption/release and writes. Read-only YAML generation supports proposed changes. Release requires applied file configuration to match the export before the revision-checked handoff | Issue #99 and subissues #100–#102: Kubernetes should support UI administration without GitOps reconciliation undoing edits | Patching the Helm ConfigMap; implicit override merging; a new CRD; immediate release to stale Git values; changing Docker defaults; declaring mutable ownership in Helm values |
| D187 | Reservations move live clients (amends D173) | A reservation (dashboard, Git or host file) may name another address than its client's live lease, provided no other reservation names the old one. That lease is not renewed and is not counted in `conflictingLeases`; the client's next DISCOVER frees it and offers the reservation, as it does for any lease the settings no longer allow. Taking another client's live address or quarantine stays refused. The dashboard's lease list can reserve a row's client | Project owner reported on 2026-10-18 that a lease could not be converted to a reservation without waiting for it to expire. Ownership of the old address is unaffected until the client gives it up, so live state is still never deleted administratively | Deleting or rewriting the live lease from the API; an administrative lease-delete endpoint; refusing until expiry |
| D188 | Independent secondary copies | Every node polls SOA and receives bounded, TSIG-authenticated TCP AXFR independently. Raw RRs stay in immutable node-local snapshots, durably committed in a versioned local.db envelope before publication. Configuration UID/spec, credential fingerprint and cluster encryption-key anchor bind the copy; absolute confirmation/expiry survives restart. Same-serial confirmation renews expiry; errors and restores never do. Keep unavailable coverage and SERVFAIL at expiry, including nested-zone CNAME targets. External serials are never stamped into shared status; node API/UI and metrics report local state. Signed secondary service, IXFR, TLS and inbound NOTIFY remain later slices | D12 primary serial ownership does not apply to externally maintained data; replicas must survive outages without asserting nonexistent names or refreshing expired data on restart | Converting AXFR into DNSRecordSet objects; leader-only receiving; shared Ready/serial implying all nodes are ready; query-time persistence/network calls; silently downgrading TLS or serving signed data without proofs |
| D189 | Inbound NOTIFY and peer hints | UDP/TCP/DoT NOTIFY reaches the authoritative handler, requires the configured primary source and optional TSIG key together, and queues a bounded identity-bound refresh. Primary hostname addresses resolve outside handlers with a two-minute authorization lifetime. The accepting node sends best-effort authenticated internal hints; recipients never rebroadcast and each independently polls/transfers. Pending hints survive an active transfer; per-zone checks are throttled to one per second. Hints never publish records or renew expiry | A load balancer can send NOTIFY to any replica without creating a leader dependency or forwarding external trust to shared zone data | Trusting the advertised serial/data; key-only source bypass; unbounded request-time resolution or peer goroutines; recursive fan-out |
| D190 | Incremental and encrypted secondary transfers | An unexpired same-credential copy supplies the base for bounded authenticated IXFR; the receiver applies deltas privately and returns one complete candidate for durable publication. Initial/expired/credential-changed copies use AXFR. Accept AXFR-format fallback or authenticated NOTIMP under one deadline/byte budget, never fallback on corrupt data/authentication failure. TLS uses system trust, mandatory endpoint hostname/IP verification and no downgrade; default port remains 53 for both protocols, with explicit :853 for standard TLS | Preserve the raw zone, serial-wrap and expiry contracts while reducing normal refresh traffic; verify every connection before trusting external data | In-place deltas; resetting limits for fallback; skipping certificate checks; renewing expired data from an old serial |
| D191 | DNSSEC provenance and response unwinding | Place postprocess after control so it wraps every answering stage; validate raw forwarded responses between cache and terminal upstream. A query pins one immutable route/trust plan and cache epoch, including aliases and detached refreshes. Cache entries carry raw validation provenance and hard proof deadlines; CD, route, trust and NTA changes isolate contexts. Validation defaults off with per-forward-zone overrides. Shipped IANA anchors plus explicit operator anchors have an operator/software-managed lifecycle; NTAs need reasons and absolute expiry within seven days. Only unmodified locally secure data can set AD | Terminal upstream cannot call a later postprocess stage; wire AD and ordinary TTLs cannot prove authenticity or bound stale security | Trusting upstream AD; caching client synthesis globally; automatic trust in fetched keys; refreshing NTA expiry on restart; RFC 5011 worker without a verified persistent lifecycle |
| D192 | Complete replication images and writer fencing | Schema 2 persists member identity and authority. Replace a validated complete manifest, objects/status and owned extension sections in one SQLite transaction; absence removes objects. Preserve source RVs, rotate local watch lineage, and expose complete-generation reads for coherent consumers. Gate every mutation and no-op with a captured writer token inside the commit boundary. Higher epochs fence; equal-epoch conflicting promotions require explicit branch selection with local quarantine | Backup Restore discards status and is not replication; API-only fencing misses controllers and files; serving readers need a generation barrier as well as atomic disk replacement | Per-object apply replication; timestamp authority; automatic conflict tie-breaking; consensus claims during network partitions |
| D193 | Stub discovery and direct authority routing | Independently discover SOA/NS/glue/SOA, pre-resolve addresses, durably store an identity-bound absolute-expiry route, then publish. UDP retries matching truncation over TCP; discovered endpoints inherit bootstrap protocol/port. RD=0 returns a local referral; RD=1 asks the captured authority with RD=0 and stops at child referrals. Preserve coverage on expiry, clear AD, bypass ordinary cache | Provide stub semantics inside the forwarding architecture with bounded queries and no request-time discovery; expose route availability per node | General iterative resolver; trusting out-of-bailiwick glue; silent TLS downgrade; restarting expiry; claiming a transferred zone or validated stub answers |
| D194 | Response synthesis outside the raw cache | Capture one immutable response plan at entry. Apply configured policy rewrites before lookup, then DNS64/AAAA/NXDOMAIN transformations after raw cache lookup, followed by response blocking and final rebinding. Nullable policy overrides use highest explicit priority; invalid compiled response policies SERVFAIL. DNS64 uses RFC 6052 layouts, at most one A dependency, conservative DO bypass and negative-TTL bounds. Modifications clear AD, AA and obsolete proofs; exemptions preserve owned zone data | Share validated raw answers without sharing client synthesis; prevent private IPv4 from hiding inside DNS64 or unchecked service hints | Caching synthesized responses; trust in upstream AD; stripping proofs before validation; unbounded alias recursion; silent fallthrough for invalid policy |
| D195 | Transferred signed-secondary serving | Preflight a single coherent NSEC/NSEC3 chain and immutable signature/proof index before publishing or restoring. DO responses assemble required live proofs with bounded work and TTLs capped by zone/signature expiry; explicit DNSSEC diagnostics retain raw stored data. Never assert AD or local signing. Invalid new data retains the old unexpired copy | Preserve externally signed answers across transfer and restart, including wildcard, alias and opt-out delegation proofs, without moving v1.0 signing into v0.2 | Serving signed data without denial proofs; cryptographic trust claims from transfer authentication; renewing signatures on SOA confirmation; query-time index construction |
| D196 | Inventory and transport client identity | Capture one bounded immutable source-batch inventory at query entry; normalize names/MACs/tags and preserve known-empty identity through dependencies. Newest observed IP claim wins, ties are ambiguous, and removal/expiry cannot restore an older identity. D86 matching unions the winning tier. Explicit host-only DoT/DoQ suffix and DoH/DoH3 path options supply client-selected IDs; authenticated PROXYv2 TLV 0xE1 carries edge IDs, and HTTP responses are private-cacheable | Make identity refresh and client policy isolation deterministic without external query-path reads; preserve catch-all precedence and distinguish selectors from credentials | Query-time UniFi/PTR calls; stale-IP fallback; trusting arbitrary HTTP/EDNS IDs; conflating inventory tags with policy group IDs |
| D197 | Integration credential capabilities | Public provider settings contain references only. Background resolution requires host-allowed exact file paths/environment names or a fixed Secret namespace; values are bounded and privately copied, while rotation status uses keyed fingerprints. Provider blocks are mutually exclusive; external writes and deletion default to disabled/Retain | Public object write access must not grant arbitrary server file or environment reads; credential rotation must not expose an offline plain-hash verifier | Inline credential values; object-selected secret namespaces; unrestricted path reads; query-time provider access |
| D198 | Generated singleton settings schema | Kudurru/default uses the existing config.Schema metadata to generate its public nested admission fields; node, host-only and secret keys are excluded. Prepare complete canonical settings against defaults before applying local environment locks; absolute blocking-disable state lives on the singleton | A replicated configuration needs one authoritative object and identical setting names/types across API, CLI, UI and admission without distributing node capabilities | Independent handwritten CRD settings schemas; arbitrary unvalidated maps; stale values after deletion; environment values copied into cluster defaults |
| D199 | Coherent serving publication and leases | Prepare a complete named component manifest from one SQLite image; publish under an opaque store-issued generation token and writer barrier. Track Installed separately from Served, gate protected capture on authority/dependency cursors, retain explicit last-good DNS-only data while fenced, and retire resources after the last request lease. Promotion pins the actual publication | Atomic disk installation alone does not make independent DNS/auth caches coherent; old in-flight work must retain its captured resources | Per-kind publication; generation counters as proof of serving; store reads per query; releasing pools immediately on reload; activating leaders before publication |
| D200 | Credential account identity | Local cookies and stored local/service-account tokens bind the immutable account UID. SQLite upgrades bind legacy tokens transactionally; timestamp ties require durable creation order, and ambiguous/orphaned credentials remain unusable. Legacy local cookies require login again | Account timestamps have one-second resolution and cannot prevent credentials passing to a recreated name; migration must not guess across a replacement | Timestamp-only identity; rebinding at authentication; accepting old unbound cookies; partial migration commits |
| D201 | Private service-account bootstrap authority | Remove builtinAdmin from ServiceAccount specs; keep privileges in private PrincipalState records keyed by full account identity and UID. Migrate existing flags transactionally and create new bootstrap accounts only when absent. A partial bootstrap never elevates a preexisting account on retry | Public CRD write access and reusable names must not grant implicit administrator privileges; Kubernetes has no transaction spanning the account and private state | Privileged public fields; trusting annotations; elevating a name found on retry; matching private state without UID |
| D202 | Committed zone aggregates | Store effective primary data, ownership/reservations, frozen policy, receipts and serial behind one CAS head with bounded immutable chunks. SQLite uses its fenced command transaction; Kubernetes uses immutable Secrets and exact head resourceVersion. Retry from current sources and prerequisites; persist NOTIFY intent with data | RFC 2136 prerequisites and mutations must be atomic even when desired record-set CRs are independent Kubernetes objects | Per-record-set UPDATE writes; substituting a current RV on a stale plan; source success presented as serving publication; unbounded aggregate decoding |
| D203 | Host credential capabilities | Stored providers resolve exact operator-granted file paths through a bounded resolver. Grants stay node-local and host-only; configured OIDC entries have a separate file capability and reserve their provider names even on failure. Resolver errors are redacted | Public object writes must not confer arbitrary host filesystem access or silently replace a configured trust authority | Direct file reads from stored paths; replicated grants; falling back to a stored issuer after config credential failure |
| D204 | Public IAM configuration | Register ServiceAccount, RoleBinding and IdentityProvider once in the public v1alpha1 model; keep credentials and PrincipalState private. SQLite schema 5 strictly converts legacy references transactionally and preserves identity/status/ownership. Kubernetes cutover requires a private offline journal and exact UID proofs | Public CRDs enable declarative IAM without publishing credential values or inheriting privileges across account replacement | Dual GVK registration; public bootstrap authority; arbitrary file reads; treating public annotations as migration authority; mixed old/new Kubernetes writers |
| D205 | Offline Kubernetes IAM cutover | Stop all readers/writers; capture bounded legacy Secrets, retain immutable source archives, create public targets and private authority with exact returned-UID receipts, then CAS a completion journal. Uncertain Creates require explicit operator recovery. Capture admission and immutable credential bindings from one verified, unchanged journal cut | Kubernetes assigns new UIDs and has no multi-object transaction; only private recorded provenance can preserve existing token identity without inheriting a recreated name | Rolling cutover; adopting matching public annotations/specs; repairing a lost UID receipt from in-memory state; name-only token rebinding; cached admission during an API outage |
| D206 | Integration ownership and durable receipts | Extend D62 ownership to cluster plus Integration UID, with destination-scoped private ledger evidence for legacy cluster-only markers. Persist immutable operation intent, reserve the exact ledger baseline, retain monotone partial receipts, and advance baseline only after confirmed completion. One complete destination plan retains per-record sync/upsert-only/create-only policy. Runtime carriers are private on both stores and present in complete replication images | Recreated Integration names must not inherit remote ownership; retries, partial provider writes and mixed publication policies must not lose or delete unrelated records | Cluster-only ownership without private evidence; preview/apply drift; publishing success from an unconfirmed provider call; separate destructive planners per policy; retrying with a fresh resource version |

| D207 | Primary lifecycle and publication | Keep private ZoneLifecycle intent/incarnations separate from the sole RR authority in ZoneState. Fence every primary ancestor before child activation; admit later ancestors against the complete current roster. Retirement tombstones the old head before replacement. Admit every UPDATE outcome against current source, credential and writer state; acknowledge wire success only after complete local publication. Recover durable NOTIFY intent with exact head acknowledgment | Parent and child heads are independent CAS objects; coverage must remain conservative during partial work, same-UID type changes and commit/publication failures | Raw desired-primary fallback; stale source RV substitution; child activation from preflight alone; head-only publication success; memory-only NOTIFY acknowledgment |

| D208 | Coherent auth views and durable session provenance | Compile auth from one full object/security image independent of DNS kind readiness. Require explicit versioned security inventories, UID-bound session enrollment, and persistent account/provider/grant floors. Lease the complete view for HTTP reads; reverify raw proof using current CommandTx for mutations and dedup. Legacy cookies require new enrollment | Per-kind caches and sealed cookies alone cannot preserve coherent identity, revocation or delete/regrant behavior across restart and promotion | Live auth fallback during captured reads; authorizing writes from a captured Principal; public-only auth images; resetting missing security history; converting scoped tokens into unrestricted sessions |
| D209 | Integration execution admission | Serialize each elected Integration UID; retain its lease barrier through root commit callbacks, and bind each provider attempt to the original source/configuration/credential cut plus durable shared request budgets. Recover uncertain remote outcomes from complete rereads; persist stable event IDs before callbacks and retain terminal facts | Provider effects cannot be atomic with local commits; rotation, failover and retries must not adopt foreign records, reset budgets or overlap old instances | Preflight-only leader checks; automatic SDK retries outside budgets; whole-database revision as source identity; blind create replay; volatile event deduplication |
| D210 | Durable TOTP consumption | Seal fixed-profile RFC 6238 seeds with exact LocalUser UID, monotonic seed version and random seed identity. Consume the greatest matching step in the same current command as session enrollment; preserve its floor through rotation, restart and promotion. Auth root v2 explicitly inventories replay rows; upgrade complete v1 state only through a startup migration | A valid code must not enroll two sessions, and a failed transaction must permit a retry without losing older revocation history | Memory-only replay caches; factor checks in an immutable read view; consuming before the session transaction; adopting legacy seeds without replay authority; resetting floors on rotation |
| D211 | Offline OIDC proof and bounded claim policy | Verify ID tokens against an immutable issuer/algorithm/JWKS resource, then evaluate scalar or bounded CEL policy at the supplied current time. Browser evaluation requires verified nonce and at_hash evidence. Return an external identity without durable UID or rights; the host separately binds exact provider UID/spec/resource and current security policy | Complete auth views cannot discover keys or reuse a verdict from another policy cut; configurable expressions need explicit type and cost limits | Network calls during view authentication; issuer-only resource matching; cached authorization verdicts; unbounded CEL; fabricated external enrollment |
| D212 | Captured external identity and session authority | Match provider ID, UID, origin, spec, policy and verifier resource in one auth image. Persist independent external account UIDs and purpose-bound session evidence under explicit auth root v3. Reevaluate current policy and intersect frozen grants with current subject authority and durable floors; strict startup migration preserves v1/v2 history | Provider replacement, unavailable config precedence, restart and promotion must not inherit stale identity or revive revoked rights; a durable session may outlive its source JWT without extending request-proof validity | Live provider fallback; synthetic local accounts; username as durable identity; automatic missing-row reprovision; unrestricted token-to-cookie conversion; decoding old manifests implicitly |
| D213 | Recoverable primary lifecycle driver | Restore a complete durable roster under its actual candidate barrier; reconcile with the exact caller's term/incarnation and pinned current background policy through every command. Acknowledge exact durable members only in one protected complete serving lease, with current routing readiness. Terminal parent release uses historical tombstone proof without rewriting that head | Restart, parent replacement and late child boundaries must converge without publishing desired data prematurely or stranding surviving children | Independent runtime snapshots as publication evidence; sampled successor terms; per-zone workers; missing-head release inference; permissive background no-ops |
| D214 | Current primary dependency exclusion | Pin publication, current immutable keys and the actual SQLite command in that order. Compile captured and transactional policy with the same owner; require the issuer's same-pin validator even for dedup/no-op, and recheck TSIG expiry at final admission. Keep startup recovery available through the writer gate while protected reads remain pending | A publication pin alone cannot exclude an independently rotating key holder, and a preflight deadline cannot authorize a later commit | Old served keys as current authority; optional policy fallback; unlocking before commit; inferring current NOTIFY sender authority from earlier key metadata |
| D215 | Authenticated UniFi inventory observations | Poll bounded complete official client assignments through the original host request budget. Preserve absolute expiry, require fresh observation after restart, use explicit stable collision allocations, and derive group contributions from MACs. Legacy metadata never renews addresses; production publication independently checks source and target ownership | Historical client records, stale IP ownership and unstable collision names cannot safely become DNS or client-policy authority | SDK-owned requests/retries; history as freshness; IP-based provider groups; invented IPv6 assignments; truncating incomplete observations; query-time provider calls |
| D216 | Durable provider source and receipt authority | Keep config-provider UIDs, source tombstones and both-origin accepted resource receipts in a distinct complete registry manifest. Reconcile complete inventories with current transactional source proof and stage every required auth floor before the registry in the same command. Initialize explicitly, validate complete images before admission, and derive serving config precedence from their registry | Host reload, provider recreation, shadowing, restart and promotion must not erase identity or revive credentials; valid individual rows cannot prove inventory completeness | Node-local invented UIDs; missing-root initialization during reads; partial receipt lists treated as complete; receipt updates before revocation floors; local config omission exposing a shadowed object; automatic history eviction |
| D217 | Acquired provider resource exclusion | Acquire bounded offline provider resources against exact durable source receipts and captured credential, trust, endpoint and entitlement policy. Keep the resource pin through current commands and complete publication; replace authority under publication, resource, independent host/license, keyring and SQLite exclusion in that order. The complete prepared cut still checks its original identities and deadlines inside the actual store publication callback. Changed desired state denies before acquisition, with private job lineage preventing A-to-B-to-A revival | Discovery, JWKS refresh and independent host replacement must not race current proof admission or revive withdrawn authority | Network authentication fallback; source hashes as acquisition provenance; accepting candidate receipts without floors; unlocking before commit; stale config lists for current commands |
| D218 | Durable global inventory evidence | Persist strict complete source revisions and original live, removed and ambiguous address watermarks under one bounded runtime root and immutable digest chunks. Initialize only at admitted empty-authority startup; require the owner in complete replica images and use same-command root CAS for replacements | Restart, promotion and source recreation must preserve expiry and prevent older address assignments from reappearing | Client-list reconstruction; refreshing timestamps on restore; missing-root fallback; payload allocation before owner bounds; independent source and inventory commits |
| D219 | Shared provider entitlement continuity | Bind one complete authority inventory to the actual cluster, verified management-policy profile and primary-admitted random activation. Reset with UID floors on startup, writer turnover and observed license replacement; a pending nonce grants nothing. Historical restarted or tainted followers await a new primary activation | Equal license bytes or local installation IDs cannot prove uninterrupted shared security authority. Policy equivalence deliberately omits signed issue/not-before and key identity; current verification still applies | Per-node durable entitlement IDs; implicit regrant of unchanged receipts; treating a random string as admitted authority; erasing follower taint on restart |
| D220 | Captured local runtime installations | Pin exact secondary, stub, RPZ and catalog installations through complete publication and current command commit. Broadcast actual changes; identical polling preserves installations. Persist catalog body/source/kind in one bounded atomic receipt | Independently changing acquisitions cannot race final admission, and a torn two-file cache cannot prove original source provenance | Fresh-pin repair of stale candidates; pointer checks without replacement exclusion; per-capture subscription accumulation; relabeling old data with a requested URL |
| D221 | Committed primary status | Project serial/hash/count from the exact durable head in a protected whole lease; compare original public/lifecycle/head tokens and frozen source evidence through the final status transaction. Defer while publication is pending, retain desired conflict diagnostics, and allocate no serial | Dynamic UPDATE data is absent from desired record sets; status writes must neither overwrite committed truth nor create an RV/readiness feedback loop | Recompiling desired records for status; lifecycle source alone as head provenance; status mutation while protected publication is pending |

---

## 17. Glossary

| Term | Meaning in this document |
|---|---|
| **Stone / clay copy** | The boundary-stone metaphor: the store that is authoritative for configuration (apiserver CRDs or `state.db`) vs. copies distributed by AXFR/IXFR/NOTIFY/catalog zones or by store replication (follower/raft). `DNSRecordSet` objects owned by app teams are also "clay copies" of authority delegated from a `DNSZone`. |
| **Store / driver** | `internal/store.Store`, the single persistence interface; drivers `kube` (CRDs), `sqlite` (`state.db`), `follower` (v0.2), `raft` (v1.0). |
| **`managedBy`** | `metadata.managedBy`: the single writer of an object (`api`, `file:<path>`, `integration:<name>`, `bootstrap`, `system`); in Kubernetes the apiserver field manager plus the `kudurru.io/managed-by` annotation. |
| **zonecache** | The always-on runnable that materialises store objects into in-memory zone trees, policy indexes and compiled lists, and persists the last-good set to `local.db`. |
| **`local.db`** | Per-node SQLite file in Kubernetes mode: last-good config snapshot, response-cache dump, (v0.2+) transferred/signed zone data. |
| **`audit-spool.db`** | Per-node SQLite file in Kubernetes mode: the hash-chained audit events of the node (D156). |
| **`state.db`** | The sqlite driver's object store in Docker mode (objects, internal kinds, change feed, journals, audit, `meta.schema_version`, `cluster_epoch`). |
| **Leader-only runnable** | A manager runnable with `NeedLeaderElection()=true`: controller, listfetcher, notifier, signer, integrations, acme. Elected by `Lease` (k8s), self (sqlite), primary (follower), raft (v1.0). |
| **Static twins (`ha-static`)** | v0.1 compose HA profile: N nodes with identical file-managed configuration, each with its own dnsdist listing all nodes, and keepalived for the VIP. |
| **Follower mode** | v0.2 compose HA: one writable primary, N read-only followers pulling snapshots; `kudurru cluster promote` with epoch fencing. |
| **Fencing / epoch** | `cluster_epoch` incremented on promote; a primary that sees a higher epoch demotes itself and refuses writes. |
| **Pipeline / stage** | The fixed 13-stage query path (`ingress … log`); stages are packages implementing `pipeline.Stage`. |
| **Status taxonomy** | The ≤ 32-value `Response.Status` enum (`forwarded`, `cached`, `blocked_list`, …) shared by the query log, metrics and UI colours. |
| **Endpoint** | The record shape: external-dns `Endpoint` fields plus a `kudurru{comment,disabled,expiresAt,view}` block. |
| **View** | `Endpoint.kudurru.view`: `public` (published to sinks, never served) or a `ClientGroup` name (served only to that group). v0.2. |
| **Publish rule / sink** | `DNSZone.spec.publish[]` selecting records for an outbound `RecordSink` integration (Cloudflare); per-sink overrides via `providerSpecific`. |
| **Ledger** | Internal kind recording what an integration wrote to a provider (`providerIDs`, `contentHash`, `generation`). |
| **Supervisor** | `kudurru edge run` / `kudurru vip run`: PID 1 in the dnsdist/keepalived images; renders, validates, execs and signals the GPL binary. |
| **Topology** | The Go struct shared by the dnsdist YAML and Lua renderers (binds, pools, backends, TLS, cache, console, webserver, rules). |
| **track_file / FIFO** | keepalived channels: kudurru writes a weight (0/60/300) to a file; keepalived writes state transitions to a FIFO that kudurru consumes. |
| **PROXYv2** | PROXY protocol v2 header prepended by dnsdist so kudurru sees the real client address; accepted only from `proxyProtocolFrom`. |
| **`health.kudurru.internal.`** | Locally answered health name (TXT `<node-id> <serial-epoch>`) used by dnsdist health checks, keepalived `track_script` and the liveness self-query. |
| **Sealed cookie** | AES-256-GCM-encrypted session cookie carrying the principal and a groups snapshot; any replica can serve it without shared state. |
| **`sessionsValidAfter`** | Per-user timestamp; sessions issued before it are rejected (password change, admin kill, disable). |
| **SAR / TokenReview** | Kubernetes `SubjectAccessReview` / `TokenReview`; used to authorise and authenticate Kubernetes identities against `kudurru.io` real and virtual resources. |
| **Virtual resource** | A `kudurru.io` resource name used only in RBAC rules (`querylogs`, `stats`, `caches`, `blocking`, `audits`, `clusters`, `settings`) with no CRD behind it. |
| **`kdr_` token** | kudurru API token: `kdr_<12-char id>_<43-char secret>`; only `sha256(secret)` is stored. |
| **hostOnly** | Config-metadata flag: the generic settings key can be set only via file/env on the host. DHCP has separate revisioned setup and live management APIs; setup requires explicit ownership and restart (D174). |
| **Conformance suite** | `internal/store/conformance`: the test suite every `Store` driver must pass (and the external-dns plan-engine cases, the blocking corpus). |
| **Rollup** | Pre-aggregated query-log tables (`rollup_1m/_1h/_1d` and per-client/qtype/upstream/domain/list/zone variants) that power dashboards without scanning facts. |
| **Scatter-gather** | Cluster-scope API calls fan out to every node's `/internal/*` with a 2 s timeout and merge results (`partial: true` on failure). |
| **Seam** | An interface or schema field that exists in v0.1 so a deferred feature can be added without a rewrite (§13.1). |
