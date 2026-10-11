# Changelog

All notable changes to kudurru are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Until v1.0 the API group is
`kudurru.io/v1alpha1` and may change between minor releases.

## [0.1.0] - unreleased (release candidate 0.1.0-rc.1)

The first release, "stone": a daily-driver DNS server that runs as one
container or as a Kubernetes Deployment with CRDs. It covers milestones M1 to
M5 of [docs/architecture.md §13.1](docs/architecture.md); the milestone
reports in [docs/m1](docs/m1/README.md), [docs/m2](docs/m2/README.md) and
[docs/m3](docs/m3/README.md) hold the measurements, and decisions D79 to D105
record what changed on the way.

### DNS serving

- One binary, `kudurru serve`, with a fixed stage pipeline: ingress,
  identify, control, local, authoritative, blocking, cache, upstream, log.
- All five transports: Do53 (UDP/TCP), DoT, DoH (HTTP/1.1 and HTTP/2), DoQ
  and DoH3, with one certificate reloader per pair.
- `SO_REUSEPORT` multi-socket UDP/TCP listeners (`listeners.sockets`, default
  GOMAXPROCS): +53 % UDP throughput.
- PROXYv2 from dnsdist, accepted only from `listeners.proxyProtocolFrom` and
  authenticated with the TLV 0xE0 shared secret (D56, D72).
- Per-prefix QPM rate limiting of UDP at ingress with TC=1 slip, then drop
  (`ratelimit.*`, D94).
- Upstream groups over dnsproxy with `parallel_best`, `strict`, `random` and
  `rtt_weighted`, conditional forwarding, forward zones, and ECS
  strip/forward/add (`upstreams.ecs`).
- Sharded TTL cache with prefetch, serve-stale (RFC 8767), in-flight
  coalescing, and a dump to `local.db` restored at start.
- CHAOS `id.server`/`version.bind`/`hostname.bind`, NSID and the
  `health.kudurru.internal.` TXT used by dnsdist and keepalived.

### Zones and transfers

- Authoritative zones from `DNSZone`/`DNSRecordSet` objects: a CoreDNS-style
  tree plus an exact index, wildcards, CNAME/DNAME chasing, query ACLs,
  NODATA versus NXDOMAIN, and SERVFAIL (never NXDOMAIN) for primary zones
  while unsynced (D80).
- Serial stamping with coalescing, `expiresAt` records, BIND zone file import
  and export.
- AXFR and IXFR out with TSIG on every transport, IXFR fallback, NOTIFY with
  retries; AXFR of 100k records in 50-80 ms.

### Blocking and policy

- Clean-room adblock/hosts/domains parser and a compiled matcher chain;
  allow-wins, `@@` exceptions, response-side CNAME inspection, block
  responses `zeroIP`/`nxdomain`/`customIP`/`refused` with EDE 15/17.
- `ClientGroup` and `Policy` objects: identification by IP, CIDR and DoT/DoH
  client ID, merged per-group policies, temporary disable (`POST
  /dns/blocking`).
- Leader-fetched blocklists pulled by replicas over the HKDF-derived TLS
  internal listener; an embedded MIT-licensed starter list.

### Store and Kubernetes

- `Store` interface with two drivers: sqlite (`state.db`, hash-chained audit,
  migrations) and kube (CRDs plus Secrets for internal kinds), sharing one
  conformance suite and the CRD validator (apiextensions + CEL, D75).
- Zone cache snapshots from store watches, cold start from `local.db` when the
  apiserver is down.
- Controller with one reconciler per kind, Events, and leader election that
  never stops the dataplane on lease loss (D44, D78).
- `objects.d` file-managed objects with fsnotify and ownership rules (D84);
  first-boot seeding.
- CRDs: `DNSZone`, `DNSRecordSet`, `BlockList`, `ClientGroup`, `Policy`; read-only
  compatibility watch of external-dns `DNSEndpoint`.

### Query log and observability

- Per-node SQLite query log (28-31 k rows/s, 141 B/row) with rollups,
  retention, privacy modes, live tail (SSE with resume) and export.
- Prometheus metrics with bounded labels, a generated Grafana dashboard and
  alert rules (`deploy/`).
- Audit trail for every write, with actor, authorization path and request id.

### REST API

- `/api/v1` on huma with an OpenAPI 3.1 document (and a 3.0 copy) committed
  with a drift test; envelope errors, keyset pagination, `If-Match`,
  `managed_by` conflicts, a route-permission lint.
- The Scalar API reference at `/api/v1/docs` is served from the binary (MIT
  bundle) under a strict CSP and honours `listeners.api.pathPrefix`.
- external-dns webhook provider (`externalDNS.webhook.*`), conformance-tested
  against external-dns v0.23.

### Authentication and authorization

- Local users (Argon2id, lockouts, first-boot admin), `kdr_` API tokens and
  service accounts, sealed session cookies, OIDC login (BFF + PKCE, tested
  against dex), Kubernetes ServiceAccount tokens authorized with
  SubjectAccessReview.
- RBAC engine with roles from `internal/rbac/permissions.yaml`; the chart's
  aggregated ClusterRoles are generated from the same file (`make
  rbac-manifests`, drift-tested). `kudurru-auditor` aggregates into no
  built-in role because it reads the query log.
- Admin socket for `kudurru admin reset-password|create-token|list-users`.

### Dashboard

- React 19 + Astryx single-page app embedded in the binary (235 KiB gzip
  initial load, 600 KiB budget); zones, records, lists, clients, query log,
  settings, nodes and tools.

### CLI

- `kudurru` with contexts and keyring token storage: `login`, `get`, `apply`,
  `delete`, `describe`, `export`, `backup`, `restore`, `zones`, `records`,
  `lists`, `blocking`, `cache`, `query` (dig-like), `stats`, `logs --follow`,
  `nodes`, `audit`, `settings`, `config`, `doctor`, `api`, `completion`.
- `kudurru token generate` prints a fresh token for
  `KUDURRU_BOOTSTRAP_TOKEN`.
- `kudurru lameduck [duration]` drains a node before shutdown (client of
  `POST /lameduck` on the ops listener). SIGTERM drains for `node.lameduck`
  (default 2s): readiness answers 503 while the listeners keep serving, so
  dnsdist and plain load balancers see the node leave before it closes.
- `kudurru doctor` checks config, data directory, ports, the API, the token
  and server diagnoses, and warns when keepalived runs without `auth_hmac`
  (`vip run --legacy`).

### Edge and VIP

- `kudurru edge run|render`: dnsdist supervisor rendering YAML (with a
  functions-only Lua sibling) or Lua, `--check-config` before every swap,
  static or EndpointSlice membership, named backends validated by the health
  check, zone-serial polling with authoritative cache flushes, dynamic blocks
  and eBPF.
- `kudurru vip run|render|drain|rebalance|fifo|probe`: keepalived supervisor
  with VRRPv3 unicast, `auth_hmac`, `nopreempt`, track files, `keepalived -t`
  before every swap, peers from a file or from Kubernetes Nodes
  (`--peers-from nodes:<selector>`).
- `kudurru health --vrrp` is the keepalived check (`kudurru vip probe` is the
  same probe).
- The server flushes the local edge's resolver cache (sidecar, compose) on
  every compiled list snapshot and every blocking toggle, so list and toggle
  changes are visible through dnsdist at once; remote edges still wait for
  the TTL (v0.2).

### Deployment and packaging

- Helm charts `kudurru` and `kudurru-crds` (CRDs kept on uninstall),
  validated over 34 value permutations with kubeconform; kustomize bases
  rendered from the chart; OIDC providers through `auth.oidc.providers`.
- Compose profiles: single instance, single instance behind dnsdist, and a
  three-node HA set with dnsdist and keepalived.
- The settings ConfigMap is read through `KUDURRU_SETTINGS_FILE` with hot
  reload, so settings changes no longer roll the pods; the compose files
  persist UI settings edits in `/var/lib/kudurru/settings.yaml`.
- goreleaser: archives, deb/rpm/apk with a systemd unit, ko images
  (`linux/amd64`, `linux/arm64`, distroless, plus `-debug`), SBOMs, cosign
  keyless signatures, `crds.yaml` and `install.yaml`; Helm charts pushed to
  `oci://ghcr.io/robertherbaugh/charts`; the `kudurru-dnsdist` and
  `kudurru-keepalived` images are built and signed on the same tag.
- Binary-size budget in CI (`make size`).

### Known limitations

See [docs/followups.md](docs/followups.md) for the full list. Notably: no
DNSSEC signing or validation (v1.0), no follower/raft replication (v0.2),
cluster-scope stats are per node, the dnsdist console diff-apply is v0.2, and
the edge and VIP behaviour still needs runs on real hardware
([docs/release-checklist.md](docs/release-checklist.md)).
