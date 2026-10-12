# kudurru

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/assets/kudurru-logo-mixed-paper.png">
  <img src="docs/brand/assets/kudurru-logo-mixed.png" width="420" height="140" alt="">
</picture>

> Babylonian boundary stones that recorded land grants and who held which territory. That’s a DNS zone: a record of authority over a defined space. The original kudurru stone was kept in a temple, and the landholder got a clay copy to prove ownership. That’s an authoritative primary with distributed secondaries, which is how DNS zone transfers work. Pronunciation is koo-DOO-roo. As a CLI it’s seven letters (kudurru).

**kudurru** is a Kubernetes-native DNS server that also runs as a single Docker container. It takes the best idea from each of the servers we admire and puts them in one Apache-2.0 Go binary:

| We took | From | What it means in kudurru |
|---|---|---|
| Replicated, API-first, one binary with roles | [Gravity](https://github.com/BeryJu/gravity) | One `kudurru` binary runs every role; configuration is replicated by the store, never by hand |
| The extreme feature set | [Technitium](https://technitium.com/dns/) | Typed zones with real SOA serials, AXFR/IXFR, NOTIFY, TSIG, DoT/DoH/DoQ, DNSSEC, per-zone access policies, apps-style integrations |
| The dashboard everyone loves | [Pi-hole](https://pi-hole.net/) | Stat tiles, 24 h charts, drill-down everywhere, a query log that explains *why* every answer was given, one-click allow/deny, and multi-node views Pi-hole never had |
| Powerful blocking | [blocky](https://github.com/0xERR0R/blocky) / [AdGuard Home](https://github.com/AdguardTeam/AdGuardHome) | hosts, domain, adblock and regex lists; client groups; per-group policies and schedules; zero-allocation matchers with a CI performance gate |
| Load balancing at the edge | [dnsdist](https://dnsdist.org) | A kudurru supervisor renders, validates and runs dnsdist in front of every node; client IPs are preserved with authenticated PROXYv2 |
| Virtual IPs on bare metal | [keepalived](https://www.keepalived.org) | A kudurru supervisor renders and runs keepalived (VRRPv3, authenticated, `nopreempt`) so the VIP follows dnsdist health |

And the things none of them had together: **Kubernetes CRDs as the source of truth in a cluster**, **OIDC login with real RBAC** (viewer, auditor, operator, zone-admin, admin; scoped per zone), and **a simple integration model** for writing records to Cloudflare (split-horizon) or pulling client inventory from UniFi.

> **Status: v0.1 release candidate in preparation (not tagged yet).** Milestones M1 to M5 are implemented: the resolver, zones and transfers, blocking with client groups, blocked services and safe search, the API, auth and dashboard, the Helm charts, compose profiles and the edge/VIP supervisors. The remaining release gates (2-VM keepalived failover, dnsdist through the edge, the signed tag) are listed in [docs/release-checklist.md](docs/release-checklist.md). See the [roadmap](#roadmap).

## How it fits together

```
                      clients (Do53 / DoT / DoH / DoQ / DoH3)
                                   │
            ┌──────────────────────▼───────────────────────┐   optional edge tier (GPL, separate container)
            │ dnsdist  :53/:853/:443   packet cache         │   PID 1 = `kudurru edge run` (render → check → exec)
            └──────────────────────┬───────────────────────┘
                     PROXYv2 (client IP preserved)  → :5353 (or :53 when no edge)
┌───────────────────────────────────▼─────────────────────────────────────────────────┐
│ kudurru node (every replica)                                                         │
│  dnsserver ─▶ PIPELINE  ingress ▸ identify ▸ control ▸ local ▸ authoritative ▸ blocking │
│                      ▸ rewrite ▸ cache ▸ dnssec-validate ▸ upstream ▸ postprocess ▸ log │
│       ▲ reads lock-free snapshots of zone trees, policies, compiled blocklists, upstreams │
│  zonecache ◀── Store.Watch ──────────────────┐  ──▶ local.db (last-good snapshot)        │
│  api (REST + UI + SSE)  querylog (ring→SQLite→rollups→sinks)  ops/metrics  internal     │
│  controller / listfetcher / notifier / signer / integrations  [leader-only]  ─┼─┐       │
└──────────────────────────────────────────────────────────────────────────────┼─┼───────┘
   STORE (config)   kube driver: CRDs via the apiserver (replicated by k8s etcd) ◀┘ │
                    sqlite driver: /var/lib/kudurru/state.db (docker)            ◀──┘
   VIP (bare metal) keepalived (VRRPv3) with PID 1 = `kudurru vip run`; in k8s: kube-vip / MetalLB / cloud LB
```

Principles that shaped it (the full list is in the [architecture](docs/architecture.md)):

- **One object model, one `Store` interface, zero consensus code in v0.1.** In Kubernetes the apiserver is the replicated store; in Docker it is embedded SQLite. Follower mode (v0.2) and raft (v1.0) arrive behind the same interface.
- **Exactly one writer per object.** Every object records who manages it (API, a file, an integration, Kubernetes), so two sources of truth are structurally impossible.
- **Serve from memory; degrade, never stop.** The query path never touches the store and keeps answering from the last good snapshot when the store is unreachable.
- **GPL stays in its own container.** dnsdist and keepalived are supervised, never linked.
- **Secure by default.** Privileged settings are host-only and can never be set through the API; OIDC logins with no matching role binding are refused, never silently admin.

## Deployment shapes

| Shape | Source of truth | HA for answers | HA for config writes | Version |
|---|---|---|---|---|
| `docker run` / compose, single node | `state.db` on a volume | none (optional dnsdist for transports and cache) | n/a | v0.1 |
| Kubernetes, multi-replica | CRDs in the apiserver | every pod serves from memory, survives apiserver outages | apiserver HA | v0.1 |
| Kubernetes, standalone ("Pi-hole on k3s") | `state.db` on a PVC | none | n/a | v0.1 |
| Compose HA, static twins | identical file-managed config on each node, dnsdist on every node, keepalived VIP | all nodes | file-managed only | v0.1 |
| Compose HA, follower mode | primary's `state.db`, pulled by followers | all nodes | primary, fenced promote | v0.2 |
| Compose HA, raft | replicated `state.db` | all nodes | any node | v1.0 |

## Quick start (planned shape; not functional yet)

Single instance:

```sh
docker run -d --name kudurru \
  -p 53:53/udp -p 53:53/tcp -p 8080:8080 \
  -v kudurru-data:/var/lib/kudurru \
  -e KUDURRU_ADMIN_PASSWORD=change-me \
  ghcr.io/robertherbaugh/kudurru:0.1.0
```

Kubernetes:

```sh
helm install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds
helm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --set replicaCount=2
kubectl apply -f config/samples/   # DNSZone, DNSRecordSet, BlockList, ClientGroup, Policy
kubectl get zones
```

The same YAML works in both modes: `kudurru apply -f zone.yaml` against a Docker instance accepts the CRD documents verbatim. Examples live in [`config/samples/`](config/samples/) and [`deploy/compose/`](deploy/compose/).

## Roadmap

| Release | Name | Headline |
|---|---|---|
| v0.1 | stone | Resolver with DoT/DoH/DoQ/DoH3, primary and forward zones with AXFR out, blocking engine with client groups, Pi-hole-grade dashboard with long-term views, OIDC + RBAC, CRDs + Helm, compose HA with dnsdist + keepalived |
| v0.2 | clay copies | Secondary/stub zones, RFC 2136, DNSSEC validation, RPZ feed import, Cloudflare split-horizon sink, UniFi inventory source, follower-mode HA, IAM and Integration CRDs, dnstap and tracing |
| v1.0 | temple | DNSSEC signing, catalog zones, RPZ publishing, raft store, ACME, Pi-hole API façade, API freeze |

Full milestones with exit criteria are in [docs/architecture.md §13](docs/architecture.md#13-roadmap).

## Documentation

- [User and administrator guide](docs/guide/README.md): how to use and run kudurru, in Simplified Technical English with screenshots.
- [Architecture](docs/architecture.md): the complete design, decision log and glossary.
- [Open questions](docs/open-questions.md): the project owner's decisions and the remaining spikes.
- [Deployment guides](docs/deploy/README.md): Docker, Helm, packages, runbooks.
- [Research notes](docs/research/): point-in-time studies of the reference projects that the architecture cites.
- [Contributing](CONTRIBUTING.md): licensing rules (no GPL or EUPL code is ever ported), layout, and how to build.
- [Branding kit](docs/brand/README.md): logos, icons, color tokens, usage rules, and original artwork.

## Repository layout

```
cmd/kudurru/        the single CLI and server binary
api/v1alpha1/       Go types for every kind; the same structs are CRDs, the SQLite schema and the REST shapes
internal/           dataplane (dnsserver, pipeline, stages, zone, blocking, cache, upstream)
                    control plane (store, controller, config, auth, rbac, api, querylog, audit, integration, edge, cluster)
charts/             Helm charts: kudurru and kudurru-crds
config/             kustomize tree, CRD bases, samples
deploy/             docker compose profiles, Grafana dashboard, Prometheus rules
images/             dnsdist and keepalived supervisor images (GPL binaries, digest-pinned)
docs/               architecture, research, open questions
web/                the React dashboard (Bun + React 19 + Astryx), embedded in the binary
site/               the documentation site (Hugo), built from docs/
```

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). dnsdist and keepalived are GPL-2.0 and are distributed only as separate, unmodified programs in their own images.
