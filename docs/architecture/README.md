# How kudurru works

A visual tour of kudurru v0.2.0: where a DNS query goes, how
a client's policy is chosen, how configuration travels from a YAML file or
the dashboard to every node, and how the deployment shapes fit together.
Every page leads with a picture and then explains it.

The optional [DHCPv4 server](dhcp.md), included since v0.1.0, runs on Linux with
either store: node-local SQLite with standalone or three-node quorum operation,
or `DHCPScope`/`DHCPLease` objects and one elected replica on the Kubernetes
store. Both have dashboard setup.

This tour is for contributors and for operators who want to know what
happens inside. For running kudurru, read the [guide](../guide/README.md)
and the [deployment docs](../deploy/README.md).
For *why* it is built this way, read the [design record](../../dev-docs/design-record.md)
and its decision log (D1 onwards); where the code and the design record
disagree, these pages follow the code and say so.

![kudurru at a glance: clients, the optional edge, one kudurru node with its pipeline, snapshot, listeners and leader-only work, the store below and the internet on the right](diagrams/overview.excalidraw.svg)

## The tour

| Page | What it shows |
|---|---|
| [A query's journey](query-path.md) | the ten pipeline stages, every way a query can leave them, and the status each outcome gets |
| [Clients, policies and blocking](policies-and-blocking.md) | how a client is identified, how its policies are merged, how one name is judged, the block page and unblock requests |
| [Block lists](block-lists.md) | how a list is fetched once by the leader, shared with every node, compiled and swapped in |
| [Upstreams and the cache](upstreams-and-cache.md) | which upstream group answers, strategies, server health, the cache and serve-stale |
| [Zones and transfers](zones.md) | authoritative answers, serials, NOTIFY and AXFR/IXFR, secondary and stub zones, stored TSIG keys and RFC 2136 UPDATE |
| [DHCPv4 and leases](dhcp.md) | the optional server, dashboard setup, staged restart apply, live management and persistent leases |
| [Configuration and the store](configuration-and-store.md) | the object model, the two store drivers, the write path, the snapshot, leader election, settings, and enrolled SQLite primaries and followers |
| [Background integrations](integrations.md) | the `Integration` object, the provider host, Cloudflare publication, UniFi inventory, Tier-B webhooks and the read-only DNSEndpoint source |
| [Deployment shapes](deployment.md) | one container, Kubernetes, compose HA with dnsdist and keepalived, and what happens when something fails |
| [Access: sign-in, tokens and RBAC](access.md) | principals, the authentication chain, OIDC sign-in, authorization, the node-to-node plane |
| [Licensing and license keys](licensing.md) | how a key is verified offline, what each state changes, and the one promise: DNS never depends on a key |
| [Observability](observability.md) | the query log pipeline, metrics, health probes and audit |
| [Stopped-source rollups](central-stopped-source-rollups.md) | how the central database owns rollups per source and recovers a stopped source |

## Five ideas that explain most of the design

1. **One binary, every node the same.** Each replica runs the whole
   pipeline, the API and the dashboard. A few loops (status, serials,
   NOTIFY, list downloads) run on exactly one node, the leader.
2. **The store is the only source of truth for configuration.** In
   Kubernetes it is the apiserver (CRDs); in Docker it is `state.db`.
   Everything else is derived from it or local to one node.
3. **The query path never touches the store.** Each node watches the store
   and builds an in-memory snapshot. Queries read the snapshot without a
   lock, and the node keeps answering from it when the store is away.
4. **The pipeline is fixed.** The stage order is code, not configuration,
   so every answer can be explained by the stage that produced it.
5. **Runtime data stays on the node.** The query log, the response cache and
   list bodies are per node; only configuration is replicated.

## One process, many loops

`kudurru serve` starts one runner. Loops in the first group run on every
node from the start; loops in the second group run only while the node
holds the leader election, and stop (without stopping the process) when it
loses it.

```mermaid
flowchart LR
    subgraph every["every node"]
        direction TB
        dns["dns listeners<br/>udp tcp dot doh doq doh3"]
        zc["zonecache<br/>(store watch → snapshot)"]
        lr["list resolver<br/>(compile lists)"]
        xfr["secondary and stub managers,<br/>RPZ feed manager (sqlite)"]
        api["api + dashboard :8080"]
        ops["health :8081, metrics :9153"]
        internal["node-to-node :8082"]
        more["settings reload, objects loader (kube),<br/>query log writer, cache sweep and<br/>persist, block page, admin socket,<br/>external-dns webhook, audit shipper,<br/>query-log shipper, central database,<br/>optional DHCPv4"]
    end
    subgraph leader["leader only"]
        direction TB
        ctrl["controller<br/>(status, serials)"]
        notifier["NOTIFY out<br/>(kube: notifier loop,<br/>sqlite: serving writer)"]
        ol["objects loader<br/>(sqlite)"]
        lf["list fetcher<br/>(downloads)"]
        jan["unblock janitor"]
        dl["allow/deny list sync"]
        ap["audit retention<br/>(state.db, AuditLog batches)"]
        cj["central database janitor<br/>(with database.dsn)"]
        de["DNSEndpoint source<br/>(kube, opt-in)"]
    end
    elect{{"leader election<br/>kube: Lease kudurru-controller<br/>sqlite: the node with write authority"}} --> leader
```

Some loops start only when their feature is on (the block page, the
external-dns webhook, the query log writer, the admin socket). There is no
`roles:` setting: every node runs the full set.

With the SQLite store the election follows durable write authority
(`internal/store/sqlite`, `Elector`): a standalone node and a static twin each
lead themselves, and in an enrolled cluster only the primary leads. An enrolled
follower, and a fenced or conflicted node, run no leader-only loop.

The objects loader, which applies `objects:` and the `objectsDir` files, is in
both groups: every replica runs it on Kubernetes, and it is leader-only on
SQLite, so an enrolled follower never applies its local files.

## Ports

| Port | Listener | Who calls it |
|---|---|---|
| 53 udp/tcp (5353 behind an edge) | DNS | clients, or dnsdist with PROXYv2 |
| 67 udp (opt-in) | DHCPv4 scopes | direct clients on UDP 68; trusted relays on their authorized source port |
| Operator-selected TCP peer port (opt-in) | DHCPv4 quorum | mutually authenticated replication and forwarded management between three voters |
| 853 tcp / 443 tcp / 853 udp / 443 udp | DoT, DoH, DoQ, DoH3 (off by default) | clients |
| 8080 | REST API `/api/v1` and the dashboard | browsers, the CLI, automation |
| 8081 | `/healthz/*`, `/lameduck` (no auth) | probes, `kudurru health`, `kudurru lameduck` |
| 8082 | node-to-node API (always TLS + token) | other replicas, edge supervisors |
| 9153 | `/metrics` | Prometheus |
| 80 (443) | block page (when enabled) | browsers sent there by a block answer |
| 127.0.0.1:8888 | external-dns webhook (when enabled) | external-dns |
| unix `/run/kudurru/kudurru.sock` | admin socket | `kudurru admin` inside the container |

## Built and not built

The design record plans more than v0.2 ships. These pages describe the
code. The main gaps between the two:

| Planned in the design record | State in the code |
|---|---|
| Complete pipeline (`local`, `rewrite`, …) | 10 installed stages, including DNSSEC, configured rewrites and the postprocess wrapper; `local` remains reserved |
| `follower` and `raft` store drivers | `store.driver` is `kube` or `sqlite`. A SQLite node can be enrolled as a primary or a follower: a follower installs the primary's complete image over member mutual TLS, and promotion is a reviewed operator command ([configuration and the store](configuration-and-store.md#sqlite-replication-foundation)). There is no `raft` store driver, and a follower is never promoted automatically |
| scatter-gather across nodes | the node list, the stats and the query log are gathered over `:8082` (or read from the central database with `database.dsn`); `/nodes/{other}/health` reads the named peer's health over `:8082` |
| cluster-wide temporary disable of blocking | replicated: `Kudurru.status.blocking` on the complete SQLite runtime (an enrolled follower forwards the command to its primary), and a `kudurru-runtime` ConfigMap annotation that every replica reconciles on Kubernetes; static SQLite twins keep node-local state ([blocking](policies-and-blocking.md#turning-blocking-off-for-a-while)) |
| client match by MAC, hostname, DoT/DoH client ID | immutable inventory matching and opt-in encrypted-transport IDs are implemented. Integrations fill the inventory on the complete SQLite runtime; DHCP lease names and the optional reverse-PTR lookup supply hostnames on both stores. Nothing fills the inventory on Kubernetes. The dnsdist edge derives the same DoT/DoQ server-name and DoH/DoH3 path IDs and sends them in the secret-authenticated TLV 0xE1 (D389) |
| `cluster.role: static` read-only twins | host-configured peers, authenticated status and file-managed SQLite twins; independent local leadership; API object and cluster-setting writes answer `503 read_only` |
| secondary zones, NOTIFY in, dynamic UPDATE | independent secondary AXFR/IXFR over TCP/TLS with authenticated NOTIFY fan-out, stub zones, and RFC 2136 UPDATE on both stores (one SQLite transaction; one UPDATE-owned record set per zone on Kubernetes). IXFR out still answers in AXFR form: there is no journal |
| DHCP server support | DHCPv4 has shipped since v0.1.0 on Linux: SQLite standalone or three-node quorum failover, or the Kubernetes store with `DHCPScope`/`DHCPLease` objects and one elected replica behind a relay; dashboard setup, direct/relayed subnets, conflict checks, advanced network/boot options, live management, lease import/export and persistent A/PTR ownership; DHCPv6 is excluded |
| Kubernetes Events from the controller | events are logged only |

The open list lives in [follow-ups](../../dev-docs/followups.md).

### What depends on the store driver

These features behave differently on the two drivers. "Complete SQLite" means
the runtime that `serve` builds for `store.driver: sqlite` (`newServingRuntime`
in `cmd/kudurru`). A 503 in the Kubernetes column is an explicit JSON refusal;
the error key is shown where it is specific to the feature.

| Feature | Complete SQLite | Kubernetes |
|---|---|---|
| Transferred RPZ feeds ([block lists](block-lists.md#transferred-rpz-foundation)) | per-node feed manager; evaluated by the blocking stage | not mounted: RPZ sources are neither transferred nor evaluated |
| Integration host ([integrations](integrations.md)) | mounted | not started; `/integrations` routes answer 503. The read-only DNSEndpoint source is separate and Kubernetes-only |
| RFC 2136 UPDATE ([zones](zones.md#rfc-2136-on-kubernetes)) | one transaction with durable primary history | one UPDATE-owned `DNSRecordSet` per zone, exact resourceVersion CAS |
| Stored TSIG key management ([zones](zones.md)) | current commands in one transaction | exact-version writes; every node rereads the source every ten seconds |
| Blocking toggle | `Kudurru.status.blocking` in the canonical store | `kudurru-runtime` ConfigMap annotation, reconciled every second |
| Cluster settings writes | canonical `Kudurru/default` commands | settings ConfigMap; only `upstreams` can move to UI ownership |
| TOTP management ([access](access.md)) | mounted | 503 `factor_management_unavailable` |
| Session inventory and revocation | durable sessions; own-session list and revoke | sealed cookies; 503 `sessions_unavailable` |
| RoleBinding write API | reviewed create, replace and delete | 503 `unavailable`; bindings are CRDs, and their revocation floors persist in the `kudurru-grant-floors` Secret |
| Local user create and password reset API | mounted | 503 `user_management_unavailable` |
| External account administration | mounted | 503 `external_users_unavailable` |
| Enrollment, join tokens and promotion | nonstatic nodes only | 503 `unavailable`; replicas share the apiserver instead |

## Reading the diagrams

Two kinds of pictures appear on these pages:

- **Hand-drawn diagrams** (`diagrams/*.excalidraw.svg`) give the big
  picture. Each SVG embeds its Excalidraw scene: open it at
  [excalidraw.com](https://excalidraw.com) (menu → Open) or with the
  Excalidraw extension for VS Code, edit it, and export it again as SVG with
  "Embed scene" checked, under the same name.
- **Mermaid diagrams** (in the markdown) show flows, sequences and states.
  GitHub and the docs site render them; edit them as text.

Colors in the hand-drawn diagrams:

| Color | Meaning |
|---|---|
| clay | kudurru code |
| blue | clients, people and tools |
| purple | dnsdist and keepalived (GPL, separate processes) |
| stone | stored data |
| yellow | leader-only work, warnings |
| green / red | an answer / a block |
| white | systems outside kudurru |
