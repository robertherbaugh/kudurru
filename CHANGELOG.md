# Changelog

Notable changes to kudurru, following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/). Until v1.0, the
`kudurru.io/v1alpha1` API may change between minor releases.

## [Unreleased]

### Added

- DHCPv4 server for v0.2.0: opt-in Linux/SQLite scopes with durable leases,
  reservations, multiple subnets and trusted relays. Node-local API and
  dashboard management apply persisted options live with revision checks.
  Native/dnsmasq lease import, native export and automatic A/PTR records
  support cutover and dedicated DHCP domains. Isolated stock-client tests
  cover acquisition, renewal, restart, release, expiration and exhaustion.
  DHCPv6 and multi-node failover remain deferred. See the
  [DHCP deployment guide](docs/deploy/dhcp.md).
- Complete DHCPv4 dashboard setup without DHCP YAML: scope creation, interface
  discovery, topology, enable/disable and explicit adoption of host-file setup.
  Desired setup is durably saved for restart; environment-owned setup stays
  locked. Per-scope revisions and lease schema v3 apply tokens preserve live
  option/reservation edits and reject stale staged changes. Removed or renamed
  scopes retain their lease databases. UI-owned DHCP activation errors keep
  DNS and the dashboard available for repair and revision-conflict recovery.

## [0.1.0] - 2026-10-16

**“stone” — the first release.** Authoritative DNS, a forwarding resolver,
blocking and a dashboard in one Go binary. Run it as one container or in
Kubernetes with CRDs as the configuration store.

### Added

#### DNS, zones and blocking

- Do53 (UDP/TCP), DoT, DoH, DoQ and DoH3; authenticated PROXYv2 from
  dnsdist, UDP rate limits and multi-socket UDP/TCP listeners.
- Primary and forward zones with query ACLs, wildcards, CNAME/DNAME,
  SOA serial management, expiring records and BIND import/export.
  Outbound AXFR/IXFR and NOTIFY; TSIG on UDP, TCP and DoT.
- Upstream groups, conditional forwarding and ECS controls; a persistent
  TTL cache with prefetch, serve-stale and in-flight request coalescing.
- Hosts, domain, adblock and regex lists, allow/deny entries, per-source
  switches and exceptions, CNAME inspection and an embedded starter list.
- IP/CIDR client groups with merged policies and schedules. A catalog of
  150 services in 14 categories supports category blocking and exceptions;
  safe search is available for supported search engines and video sites.
- An optional block page with access requests and administrator approvals.

#### Dashboard, API and CLI

- An embedded React dashboard with node and cluster views, 24-hour charts,
  long-term statistics, query explanations, live tail and client inventory.
  Pages and wizards manage zones, records, upstreams, lists, groups,
  policies, allow/deny entries and access requests.
- A REST API with OpenAPI references, generated clients, pagination,
  conditional writes and object ownership checks; an external-dns webhook
  provider writes `DNSRecordSet` objects.
- CLI commands for object management, queries, logs, statistics, blocking,
  cache, settings, backup/restore and diagnostics; contexts and keyring
  token storage; an admin socket for account recovery.
- Local accounts, API tokens and sealed sessions; OIDC with PKCE,
  zone-scoped roles and Kubernetes ServiceAccount authorization through
  SubjectAccessReview. Feature entitlements are described below.

#### Stores and observability

- SQLite and Kubernetes stores, file-managed objects, first-boot defaults,
  leader election and in-memory serving snapshots. Kubernetes nodes can
  start from saved snapshots during an apiserver outage.
- Per-node query logs with retention, privacy controls, rollups and export;
  node/cluster reads and cluster live tail.
- Persistent audit trails with integrity verification. Kubernetes replicas
  share retained events through immutable `AuditLog` objects.
- Optional [central PostgreSQL](docs/deploy/postgres.md) for audit, query
  logs and statistics, including data from replicas that are down or gone.
  Nodes buffer data locally during database outages.
- Prometheus metrics, a Grafana dashboard, alert rules and health probes.

#### Deployment and documentation

- Helm charts for the server and CRDs, kustomize overlays, and Compose
  profiles for one node, one node behind dnsdist and three independent
  file-managed nodes with a keepalived VIP.
- dnsdist and keepalived supervisors with validated configuration,
  backend discovery, encrypted transports and VIP drain/rebalance tools.
  Keepalived supports multiple VIPs and separate VRRP interfaces; the
  dashboard reports supervisor health and VIP state across nodes.
- Linux/macOS binaries, Linux packages with systemd, multi-architecture
  container images, debug and host-network variants, deployment bundles,
  SBOMs and signed release artifacts.
- Persistent settings edits in Compose and settings-file hot reload;
  Helm configuration changes still roll pods through ConfigMap checksums.
- Traefik ingress with sticky sessions and optional API Service affinity.
- The [user and administrator guide](docs/guide/README.md), architecture
  tour and generated settings, CRD, CLI and API references at
  [docs.kudurru.io](https://docs.kudurru.io), plus the project website at
  [kudurru.io](https://kudurru.io).

### Fixed

Fixes incorporated during release-candidate testing:

- Dashboard/API mismatches affecting settings, query results, clients,
  nodes, audit, token creation, zone import, password changes, sign-out,
  live-tail resume and deep links. Client inventory and charts now follow
  the selected node or cluster scope; navigation shows version and plan.
- Zone deletion no longer leaves orphaned records. Record writes retry
  status-update races, duplicate zone apexes no longer freeze unrelated
  updates, and BIND import/export preserves escaped SOA names.
- Kubernetes startup no longer blocks saved-snapshot DNS serving on
  unavailable bootstrap, identity discovery or audit permissions. Startup
  retries after recovery while preserving authentication and revocations.
- Kubernetes audit records survive replica restarts. Consistent audit
  snapshots prevent false tamper reports during retention; the rotation
  runbook retains keys needed by stored audit batches.
- Cloudflare DoT refusals retry once on a fresh connection; conditional
  forwarding, readiness and shutdown-drain behavior are corrected.
- Fresh Docker volumes have writable data directories; generated Helm
  bootstrap tokens use valid IDs. Edge image builds, supervisor arguments,
  keepalived restart handling and host-network/eBPF permissions are fixed.
- Fresh Kubernetes installs seed defaults in the release namespace;
  existing installs preserve their objects in `default`. The
  `database.clusterDNS` chart option resolves in-cluster database Services.

### Security

- Hardened list and catalog fetching against unsafe URLs and redirects;
  bounded query-log memory and list-body parsing; corrected unblock-request
  privacy, approval races and managed-list write handling.
- Fixed authentication-cache revocation races and enforced required
  password changes. Password resets can also revoke API tokens.
- Release builds use explicit tag refs, pinned actions/tools, separate
  read-only test jobs and restricted signing identities. Promotion requires
  a successfully published candidate.

Details and validation are in the [security reviews](docs/m5/README.md)
and [final feature review](docs/m5/features-v0.1.md).

### License

- Core DNS and the dashboard use the [Kudurru Community License](LICENSE).
  Eligible personal, homelab, nonprofit and isolated community-development
  uses are permitted; business and government production requires an
  accepted commercial agreement and Order Form. Folder-specific and
  third-party terms remain separate; historical grants remain in effect.
- Without a key, kudurru supports one node, the default policy and the
  built-in administrator. A free eligible community key covers up to three
  nodes and unlocks additional policies, OIDC, scoped RBAC, audit storage,
  long-term statistics, unblock requests, edge supervisors and integrations.
  Commercial entitlements follow the agreed plan or modules.
- Keys are verified offline; DNS answering never depends on a key. The
  separate [license portal](license-server/README.md) issues keys after
  approval; no kudurru node contacts it. See [LICENSING.md](LICENSING.md)
  for terms and feature limits, or [request a key](https://kudurru.io/license).

### Installation and upgrade notes

- Pin Docker images and Helm charts to `0.1.0`. Install or upgrade
  `kudurru-crds` before the server so `UnblockRequest` and `AuditLog` are
  available. Start with one replica without a key; multiple replicas need
  an entitlement to permit API/dashboard configuration writes.
- Back up data and encryption keys before upgrading from a candidate.
  Review the [Helm guide](docs/deploy/helm.md) for namespace defaults and
  the [runbooks](docs/deploy/runbooks.md) for key rotation and recovery.

### Known limitations

- Secondary/stub zones, inbound transfers, RFC 2136, DNSSEC validation,
  Cloudflare/UniFi integrations and follower replication are planned for
  v0.2. DNSSEC signing and raft replication are planned for v1.0.
- Client matching uses IP/CIDR. MAC, hostname and DoT/DoH client-ID fields
  have no production inventory or listener source yet; preserve device
  source addresses through gateways and load balancers.
- Compose HA nodes have independent SQLite stores; distribute identical
  file-managed configuration yourself. Temporary blocking toggles are
  node-local, and remote edge caches wait for TTL expiry after list changes.
- IAM write pages, remote node health, dnsdist console diff-apply and
  cluster-wide blocking toggles remain future work.
- Recorded local checks do not certify real-network client identity or
  two-VM VIP failover. Verify these in your deployment using the
  [release checklist](docs/release-checklist.md) and runbooks.

See the [architecture tour](docs/architecture/README.md#built-and-not-built)
and [follow-ups](docs/followups.md) for the full scope and backlog.
Measurements and development history remain in the
[milestone reports](docs/m5/README.md).
