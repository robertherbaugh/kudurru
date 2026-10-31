# Changelog

Notable changes to kudurru, following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/). Until v1.0, the
`kudurru.io/v1alpha1` API may change between minor releases.

## [Unreleased]

### Changed

- DHCP: a reservation can move a client that leases another address
  instead of being refused until the lease expires; the client moves to it
  when it next renews or reconnects. **DHCP** > **Leases** has a **Reserve**
  action that prefills the reservation from a lease.

### Fixed

- Cache capacity reload now adjusts shard topology with bounded migration and
  preserves entry scope, security metadata and absolute deadlines.
- Remove UniFi SDK initialization of fake-data templates from production,
  restoring linker dead-method elimination without changing request checks.

- Docker: the compose files and the `docker run` examples now pass `serve`.
  The 0.1.0 image has no default command, so as shipped the container
  printed the CLI help, exited and restarted in a loop.

## [0.2.0] - unreleased

### Added

- Helm: provider annotation parity for mixed/split DNS Services across edge
  topologies; kube-vip avoids implicit MetalLB sharing, and Service VIP
  providers require a LoadBalancer DNS Service.

- DNS: TSIG verification and signed responses on DoH, DoQ and DoH3, with
  original signature deadlines and no-store HTTP replies. Encrypted transports
  still cannot authorize native UPDATE or NOTIFY mutations.
- Central history: source-owned schema-5 rollups recover stopped sources from
  bounded retained input, preserve late returns and expose scoped recovery
  evidence. Legacy history without provable ownership remains intact with an
  explicit provenance gate; older replace-upsert writers are refused.

- Domains: read-only concrete-name regex inspection with opposing list/rule
  evidence, scope, disabled state and explicit partial-result limits.

- Source-entry browsing: bounded digest/parser indexes avoid repeated parsing
  of large downloaded lists while preserving exact exclusions, line numbers,
  original-publication cursors and body verification.

- Blocking: refresh interval and degraded readiness reload without restarting
  in-flight downloads. New lists inherit the cluster refresh default unless
  they specify an interval; existing stored durations remain overrides.
- Interoperability: pinned Knot DNS 3.6.2 acceptance for ordinary primary-zone
  RFC 9432 catalog data, automatic member replacement and transfer refusals.

- Doctor: opt-in authenticated self-NOTIFY reports the listener-observed UDP
  source address on the complete SQLite path, with bounded work and explicit
  loopback limitations.
- Client groups: optional UDP rate-limit exemption with captured membership,
  generated CRD validation and an ownership-aware dashboard control.

- Upstreams: bounded dnsmasq/dnsproxy routing-file import into canonical
  groups, with unsupported-line reports, collision refusal and ownership checks.

- Central history: read retained data for retired nodes without live peer
  membership, with exact-node statistics, dictionaries and export.
- DNS over TLS: use configured Linux listener sockets, retaining TLS rotation
  and existing connection limits.

- Query log: download a filtered JSON page of up to 1,000 retained queries,
  preserving privacy, source, continuation and partial-node metadata.

- Kubernetes audit: independent persisted signing and retained verification
  keys, explicit rotation/revocation and trust-aware verification caching.
  Existing AuditLog deployments require the documented key-provisioning and
  legacy-cutover procedure before upgraded cluster shipping resumes.
- File objects: live reload of the node configuration’s inline objects, with
  whole-source validation, last-good retention and environment precedence.

- Helm: explicit per-VRID HMAC Secret references, strict complete mapping,
  private key staging and coordinated restart guidance for keepalived.
- Domain checks: matching disabled rules come from the query's captured lists
  and client policy, with bounded evidence and no second browser request.
- CLI: reviewed zone-option get/set commands with resource-version checks,
  complete-spec replacement and the existing server ownership/authority checks.
- Audit: dedicated authenticated SQLite authorization/request/client columns
  and shared credential-field redaction before new document persistence.
  Legacy audit bytes and verification remain compatible through schema 6.
- Storage: independent query log and local snapshot directories with additive
  Helm/Compose volumes, path preflight and explicit offline migration.
- Downloads: opt-in host HTTPS-only policy for lists and catalogs, including
  redirect downgrade refusal, without changing existing cached data.

- Central query storage: bounded reference-safe dictionary pruning, guarded
  cached-ID reuse and transactional schema-4 upgrade constraints. Retained
  facts and rollups keep their dictionary entries.
- Acceptance tooling: disposable Samba GSS-TSIG protocol and two-node
  dnsdist/keepalived Docker labs, plus a Windows provisioning prompt.
- CLI: actual-router conformance for apply outcomes, settings, pagination and
  live query events, with documented wire contracts.

- DNS controls: hot-reloadable allowed networks and blocked query types,
  including DNS64 dependencies, with validated settings and immutable rules
  for each captured request.

- SQLite integrations: mount licensed Cloudflare and UniFi background execution
  with per-instance election, original credential/writer fencing, atomic source
  inventory and publication-qualified DNS/group updates. Retain is supported;
  Kubernetes atomic-source execution and Delete cleanup remain pending.

- Recursive DNS: negotiate per-authority DNS cookies with bounded peer state,
  address-bound random client cookies, rotation and budgeted BADCOOKIE recovery.
  Cookies stay out of shared response caches and client answers.
- Zone reconciliation: bounded per-record-set parsing reuse with detached RRs
  and identity, TTL and expiry invalidation across controller and primary builds.

- Zones: select a separate NOTIFY TSIG key and local source address through
  zone policy and primary-zone wizard/settings controls. Missing keys and bind
  failures refuse delivery without switching identity or source.
- Recursive DNS: expiring negative trust anchors with independent configuration,
  absolute cache deadlines and preserved validation of alias targets outside
  the exception. Reload and restore do not extend authority.
- Dashboard: self-hosted Inter and JetBrains Mono with normal/italic variable
  weights, bundled license notices and system fallbacks. Font payloads count
  toward the unchanged initial-load size budget.

- Encrypted DNS: negotiated RFC 8467 response padding on DoT, DoH, DoQ and
  DoH3, bounded by the client's payload limit and preserving TSIG signatures
  and transfer chains. Shared cache entries remain independent of padding.

- Diagnosis and doctor: bounded AuditLog API availability and namespace batch
  counts, gated by audit permission and license. Incomplete scans remain explicit;
  healthy informational diagnoses no longer make doctor report a warning.

- DoH: advertise an active matching native HTTP/3 endpoint with Alt-Svc while
  preserving its address, port, certificate, query path and client-ID policy.
  Reverse proxies remain responsible for their public advertisement.

- Kubernetes recovery: independently configured cluster identity pins initial
  online access and saved DNS/settings admission. Offline recovery requires
  `store.kube.expectedClusterID`; legacy identity-free state needs live refresh.
- Kubernetes authentication: require projected audience credentials with at most
  ten minutes of original lifetime, retain expiry through authorization, and
  bound complete TokenReview acquisition. Legacy and hour-long tokens are
  refused; use a rotating audience `kudurru` projection with 600-second expiry.

- Query-log privacy: exclusion-only reloads preserve daily client pseudonyms,
  including across midnight; privacy mode/method changes and restart keep their
  separate key lifecycles.

- Native DNS listeners: host-only, restart-required TCP query-count, QUIC
  connection-count and TCP/HTTP/QUIC idle-timeout settings. Existing unlimited
  count defaults and idle durations are preserved.

- Store conformance: shared invalid-input checks on SQLite and Kubernetes;
  invalid forced Kubernetes updates are admitted in dry-run before ownership
  changes, with consistent structured validation errors.

- Health: the human `/healthz` response includes the locally observed controller
  leader and the age of the saved snapshot currently being served.
- SQLite replication: `kudurru_cluster_fenced` reports durable and failed
  in-memory authority fences, with an immediate Prometheus alert and Grafana
  panel. Healthy read-only followers do not trigger the fencing alert.

- Independent secondary-zone AXFR/IXFR ingestion over TCP or verified TLS, with optional TSIG,
  SOA refresh/retry/expiry, RFC 1982 serial handling and durable node-local
  last-good copies. Invalid, unauthenticated and oversized transfers retain
  previous data; expired or unavailable secondaries answer SERVFAIL.
- Secondary configuration in the zone wizard/settings and node-specific
  serial, availability, expiry and failure reporting in the API, dashboard
  and metrics. Transferred records remain read-only.
- Authenticated health queries for remote cluster members, with bounded
  requests and explicit unavailable or incompatible-peer errors.
- Offline AdGuard Home and Blocky CLI import previews, selection and tracked
  application, with bounded YAML input and explicit conversion-loss reports.
- `kudurru audit verify`, including structured results and nonzero exit status
  for integrity failures, unavailable verification or denied access.
- Accessible help for nested settings list items, map keys and values.
- Optional file-backed bearer authentication for the external-dns webhook,
  with bounded startup loading and restart-required rotation.
- A rate-limit slip counter and Grafana panels for central database
  availability, query shipping lag and audit spool drops.

### Fixed

- Edge health: bypass dnsdist packet caching for the exact health-query name
  so a transient lameduck failure cannot keep a healthy VIP in FAULT after
  the draining backend has been removed.

- Zone import/export: preserve non-ASCII DNS label octets and canonicalize SOA
  and apex nameserver names so repeated BIND export stays byte-identical.

- SQLite administration: restore current domain, Allowlist, Denylist, query-log
  actions and unblock decisions with atomic list/policy effects and final
  credential and license checks. Domain and unblock reads retain one captured
  publication, including compiled lists, settings and finite entitlements.
- Migrations: restore apply, background import and authority cutover; tracking
  previous imports now atomically checks every reviewed zone UID and version.
  Selected settings commit with their durable import result, preserve map
  deletion/reset semantics and report restart requirements. SQLite authority
  cutover completes its report in the final zone transaction, preventing a
  fully applied cutover from retaining an unfinished report.
- Primary publication stops waiting for an obsolete desired source, allowing
  reconciliation after authority changes without serving stale data.

- CNAME/DNAME chasing rechecks the most specific local zone before following
  a target, so parent wildcard data cannot bypass a child zone's data or expiry.
- Current SQLite commands recheck verified credential expiry at final admission,
  including no-op and dedup results. Captured primary routing now enforces the
  earliest ancestor expiry without changing durable head evidence.

Stub zones and signed secondary serving are implemented on the development
branch. Inbound NOTIFY verifies the primary source
and configured TSIG, schedules bounded local refreshes, and fans authenticated
hints to peers so every replica transfers independently.

## [0.1.0] - 2026-10-18

**“stone” — the first release.** Authoritative DNS, a forwarding or
validating recursive resolver, blocking, optional DHCPv4 and a dashboard in
one Go binary. Run it as one container or in Kubernetes with CRDs as the
configuration store.

### Added

#### DNS, zones and blocking

- Do53 (UDP/TCP), DoT, DoH, DoQ and DoH3; authenticated PROXYv2 from
  dnsdist, UDP rate limits and multi-socket UDP/TCP listeners.
- Primary and forward zones with query ACLs, wildcards, CNAME/DNAME,
  SOA serial management, expiring records and BIND import/export.
  Outbound AXFR/IXFR and NOTIFY; TSIG on UDP, TCP and DoT.
- Upstream groups, conditional forwarding and ECS controls; a persistent
  TTL cache with prefetch, serve-stale and in-flight request coalescing.
- An opt-in recursive resolver: an upstream group with `mode: recursive`
  resolves names from the root servers with QNAME minimization and DNSSEC
  validation (bogus answers become SERVFAIL with an extended DNS error and
  the `dnssec_bogus` status), never sends a client subnet and answers
  private reverse zones and special-use names such as `home.arpa` locally.
  Settings are under `upstreams.recursion`; forwarding to 1.1.1.1 and
  9.9.9.9 stays the default.
- Primary zones can opt into missing-name fallback to a forwarding or native
  recursive upstream group. Local names keep their own answers, including
  missing-type responses; dashboard zone creation and settings expose the
  option. Existing primary zones remain authoritative by default (#95).
- Hosts, domain, adblock and regex lists, allow/deny entries, per-source
  switches and exceptions, CNAME inspection and an embedded starter list.
- IP/CIDR client groups with merged policies and schedules. A catalog of
  150 services in 14 categories supports category blocking and exceptions;
  safe search is available for supported search engines and video sites.
- An optional block page with access requests and administrator approvals.

#### DHCPv4

- DHCPv4 server: opt-in Linux/SQLite scopes with durable leases,
  reservations, multiple subnets and trusted relays. Node-local API and
  dashboard management apply persisted options live with revision checks.
  Native/dnsmasq lease import, native export and automatic A/PTR records
  support cutover and dedicated DHCP domains. Isolated stock-client tests
  cover acquisition, renewal, restart, release, expiration and exhaustion.
  Three-node quorum replication provides automatic DHCPv4 failover; DHCPv6
  is excluded. See the [DHCP deployment guide](docs/deploy/dhcp.md).
- Complete DHCPv4 dashboard setup without DHCP YAML: scope creation, interface
  discovery, topology, enable/disable and explicit adoption of host-file setup.
  Desired setup is durably saved for restart; environment-owned setup stays
  locked. Per-scope revisions and durable apply tokens preserve live
  option/reservation edits and reject stale staged changes. Removed or renamed
  scopes retain their lease databases. UI-owned DHCP activation errors keep
  DNS and the dashboard available for repair and revision-conflict recovery.
- Three static DHCP voters with mutual TLS, majority-committed leases and
  management writes, follower forwarding, leader replacement, snapshot catch-up
  and recovery fencing. The dashboard configures peers and reports quorum state.
- Bounded ARP/ICMP pre-offer conflict detection, trusted relay link selection,
  server-ID override and alternate ports; NTP/search/classless-route options,
  architecture-specific PXE/HTTP boot settings and client FQDN handling.
- DHCPv4 on the Kubernetes store: `DHCPScope` objects (from the dashboard or
  from Git with `kudurru.io/managed-by`) and `DHCPLease` objects
  (`dhcp.kudurru.io`) replace node-local setup and lease files. One replica,
  elected through the `kudurru-dhcp` Lease, serves relayed clients at a
  LoadBalancer address (`dhcp.enabled`, `dhcp.serverAddress` in the chart);
  leases survive failover, and every replica shows the same DHCP pages and
  answers lease DNS names. The dashboard's DHCP pages no longer depend on
  which replica a sticky session reaches. Install the `kudurru-crds` chart
  first. In Kubernetes RBAC, the `dhcp` permission is now the `dhcpscopes`
  resource.
- Persistent earliest-established DHCP DNS name ownership across scopes,
  with reservation precedence and import-safe claim ordering. Indexed lease
  transactions and incremental DNS publication keep renewals fast: median
  renewal time is 0.35 ms at 10,000 leases in the FULL-synchronous SQLite
  fixture.

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
- Explicit UI or GitOps ownership for upstream settings in Kubernetes, with
  shared persistence, revision-checked adoption, YAML previews, a checked
  handoff back to GitOps, dashboard controls, and CLI commands. Existing
  Kubernetes installs stay GitOps-managed; Docker settings remain editable.

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
- Short, illustrated task guides for every dashboard page, with a page index,
  step navigation and full-size screenshots. Separate Linux and Kubernetes
  DHCP walkthroughs cover host preparation, router relays, required trusted
  addresses, safe cutover and lease verification.
- The website and docs-site footers link to the license portal
  (licenses.kudurru.io) and the auth portal (auth.kudurru.io).

### Changed

- The repository layout names each part by what it is: the dashboard moved
  from `web/` to `dashboard/`, the kudurru.io website from `www/` to
  `website/` and the docs site from `site/` to `docs-site/`. The Helm charts
  (`charts/`) and the kustomize tree (`config/`) moved under
  `deploy/helm/` and `deploy/kustomize/`. A remote kustomize overlay is now
  `github.com/robertherbaugh/kudurru/deploy/kustomize/overlays/<name>`;
  overlays pinned to commits before this move keep the `config/` path.
  `docs/` keeps the published documentation; the design record (formerly
  `docs/architecture.md`, now `dev-docs/design-record.md`), milestone
  reports, research, spikes and follow-ups moved to `dev-docs/`. The
  `make web-*` and `make www-*` targets are now `make dashboard-*` and
  `make website-*`.
- API migrations run as tracked background jobs with selectable objects and
  settings. `kudurru import pihole|technitium --apply` now exits successfully when
  the server accepts the job, even if items later fail. Existing scripts that
  require a final success/failure exit code must add `--wait`. Inspect saved
  results with `kudurru import runs RUN_ID` or the dashboard's migration history.

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
- Statistics no longer drop queries that arrive for an hour that was already
  summarized (late events, or a new query log in its first minutes): until
  the hour is summarized again, the summary, rankings and time series, and
  the hourly rollups sent to the central database, use the per-minute data.
- A token, user or other object written while a node re-reads its object
  cache is no longer dropped from that cache until the next re-read, so a
  new API token works on that node at once.

### Security

- Hardened list and catalog fetching against unsafe URLs and redirects;
  bounded query-log memory and list-body parsing; corrected unblock-request
  privacy, approval races and managed-list write handling.
- Fixed authentication-cache revocation races and enforced required
  password changes. Password resets can also revoke API tokens.
- Release builds use explicit tag refs, pinned actions/tools, separate
  read-only test jobs and restricted signing identities. Promotion requires
  a successfully published candidate.

Details and validation are in the [security reviews](dev-docs/milestones/m5/README.md)
and [final feature review](dev-docs/milestones/m5/features-v0.1.md).

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
- The license portal holds Order Forms: license admins draft one for a
  customer or from a business request, with the fees of the price list,
  and send it; an authorized contact of the customer accepts it (or
  declines it) signed in, a license admin countersigns it, and the license
  is issued from the executed form. Both parties keep the signed copy.

### Installation and upgrade notes

- Pin Docker images and Helm charts to `0.1.0`. Install or upgrade
  `kudurru-crds` before the server so `UnblockRequest` and `AuditLog` are
  available. Start with one replica without a key; multiple replicas need
  an entitlement to permit API/dashboard configuration writes.
- Back up data and encryption keys before upgrading from a candidate.
  Review the [Helm guide](docs/deploy/helm.md) for namespace defaults and
  the [runbooks](docs/deploy/runbooks.md) for key rotation and recovery.

### Known limitations

- Secondary/stub zones, inbound transfers, RFC 2136, DNSSEC validation of
  forwarded answers, Cloudflare/UniFi integrations and follower replication
  are planned for v0.2. DNSSEC signing and Raft replication of the DNS
  object store are planned for v1.0. DHCPv4 quorum replication is included.
- Recursive groups need outbound UDP and TCP port 53 to any address. They
  do not yet cache NSEC ranges aggressively (RFC 8198), follow root key
  rollovers automatically (RFC 5011; new anchors ship with releases or
  `upstreams.recursion.trustAnchors`), support negative trust anchors or
  DNS cookies, or serve a local copy of the root zone (RFC 8806).
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
  [release checklist](dev-docs/release-checklist.md) and runbooks.

See the [architecture tour](docs/architecture/README.md#built-and-not-built)
and [follow-ups](dev-docs/followups.md) for the full scope and backlog.
Measurements and development history remain in the
[milestone reports](dev-docs/milestones/m5/README.md).
