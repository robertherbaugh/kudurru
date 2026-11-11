# Changelog

Notable changes to kudurru, following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/). Until v1.0, the
`kudurru.io/v1alpha1` API may change between minor releases.

## [Unreleased]

### Preview

- Active Directory DNS replacement remains unfinished preview work. The owner
  deferred the remaining implementation and real Windows qualification to a
  future effort; no production AD replacement or Windows support matrix is claimed.

### Added

- RFC 2136 dynamic updates on Kubernetes. Each zone's updates live in one
  `rfc2136-<zone>` DNSRecordSet written by exact resourceVersion CAS, so concurrent
  UPDATEs on any node are serialized and a prerequisite failure writes nothing.
  The same key x owner x type grants apply as on SQLite, and records owned by
  API, file or integration sources cannot be changed by an UPDATE.

- Stored TSIG keys can be created, replaced, resealed and revoked through the API
  on Kubernetes, and every node reloads the stored source without a restart. A
  changed source that cannot be prepared withdraws signing rather than keeping a
  revoked key.

- Local password login on an enrolled SQLite follower is admitted by the
  primary under the shared lockout counters; the follower keeps the session's
  CSRF secret, forwards only its hash and seals the cookie once it installs the
  session. The password is never persisted.

- Enrolled SQLite followers forward canonical object writes and deletes
  (zones, record sets, block lists, client groups, policies) and the zone
  convenience commands (record upsert/delete, import, zone delete) to their primary.
  The primary reruns the public route's own validation and command, re-authorizes
  the original credential and commits effect, audit and deduplication together;
  the follower returns its exact result or refusal and never retries.

- Explicit offline branch recovery after conflicting SQLite promotions:
  `cluster resolution --offline` reviews a stopped member's recorded conflict and
  `cluster resolve --offline` keeps that member's branch at a new epoch, with the
  prior rows quarantined and a `cluster.resolve` audit event in the same
  transaction. Other members adopt it through scheduled peer exchanges and never
  replay their discarded writes. Three-process acceptance passes.

- Enrolled SQLite primaries periodically exchange authenticated peer terms. A
  returning old primary fences its writes before a later independent follower
  pull installs the successor image. Two-process promotion and rejoin acceptance
  passes.

- Enrolled SQLite followers forward blocking toggles once to their original
  primary with retained source, credential and deadline checks. HTTP 202 reports
  a confirmed primary commit with serving publication pending; the dashboard and
  CLI keep that receipt separate from observed node state.

- Offline `cluster join` for a pristine SQLite receiver, using an original
  invitation-pinned TLS route, signed member possession, atomic acceptance audit,
  and atomic first-image/authority installation. Failure retains the local
  identity. Explicit `cluster join-reconcile` recovers an incomplete join through
  retained intent and current member TLS without resending its proposal. Full
  HA acceptance remains open.

- Reviewed enrolled-follower promotion through the API and `cluster promotion` /
  `cluster promote`, with original credential/source checks, atomic audit,
  bounded body/queue lifetime and explicit unknown-completion handling.

- Retained enrolled SQLite followers now acquire and publish complete images
  from their primary over mutual TLS, with bounded cancellation and joined shutdown.

- `cluster join-token` issues a reviewed, expiring member invitation through the
  authenticated SQLite API, with atomic audit and a bounded secret response
  emitted only after confirmed commit.

- `cluster enrollment` and reviewed `cluster activate` initialize the first
  SQLite HA primary through the authenticated API, with atomic audit and an
  explicit restart-required receipt.

### Changed

- DHCP: a reservation can move a client that leases another address
  instead of being refused until the lease expires; the client moves to it
  when it next renews or reconnects. **DHCP** > **Leases** has a **Reserve**
  action that prefills the reservation from a lease.

### Fixed

- Kubernetes: removing a RoleBinding now durably revokes earlier sessions'
  frozen grants on every replica. Replicas share revocation floors in a
  `kudurru-grant-floors` Secret, so a restart or a replica that missed the change
  cannot revive an old session after the binding is granted again.

- A fenced or conflicted SQLite node now publishes its last-good data as a
  DNS-only bundle and keeps answering DNS from it. Previously its preparation
  was always checked as a writable publication and failed, so the retired bundle
  answered SERVFAIL until the node installed a successor.

- Honor promotion cancellation while waiting behind an active member exchange.

- Honor follower-pull cancellation while waiting behind another acquisition.

- Avoid redundant startup image validation on SQLite nodes while retaining
  complete source checks, original readiness deadlines and final admission.

- Reduce lock contention for parallel response-cache hits while preserving
  complete flushes, resizes, restored scopes and expiration checks.

- DNS over QUIC and HTTP/3: synchronize connection state during path migration
  and capability reads when the sender disables UDP segmentation offload.

- SQLite followers awaiting their first image keep operational listeners alive
  without activating member routes or inventing enrollment history.

- Provider acquisition retains source changes that arrive during reconciliation
  and stops revalidating unchanged successful images on every retry tick.

- Cluster failover: a verified primary that aborts HTTP remains authenticated
  contact, so response refusal cannot create a missed-pull promotion window.

- Cluster transport: add enrolled replication routes that retain original peer
  admission through command completion and explicit HTTP response flushing.
  Production listener, domain policy and operator-route assembly remain open.

- Cache capacity reload now adjusts shard topology with bounded migration and
  preserves entry scope, security metadata and absolute deadlines.
- Remove UniFi SDK initialization of fake-data templates from production,
  restoring linker dead-method elimination without changing request checks.

- Docker: the compose files and the `docker run` examples now pass `serve`.
  The 0.1.0 image has no default command, so as shipped the container
  printed the CLI help, exited and restarted in a loop.

## [0.2.0] - unreleased

### Fixed

- Cluster storage: retain original peer enrollment, certificate and request
  lifetimes through image/command admission and bounded output exclusion.
  Root HA routes and domain authorization mounting remain in progress.

- Cluster storage: initial membership and Primary activation now share one
  authorized transaction with audit and final lifetime checks. Operator routes
  and production HA assembly remain in progress.

- Add bounded member enrollment state and one-time invitation wire formats with
  key-possession proof, original expiry and permanent signing-key revocation.
  Schema 9 persists complete membership with atomic consumption, final admission
  checks and irreversible replica history; join-token routes remain pending.

- The enrolled replication client verifies the original member identity before
  sending credentials or commands and retains original deadlines through result
  admission. TLS refusals do not count as primary network loss.

- Add persistent, independent member TLS identity primitives for enrolled HA;
  enrollment and production transport mounting remain pending.

- The HA promotion coordinator retains the original operator authorization
  through SQLite commit and idempotent success; late expiry, revocation or
  cancellation rolls back the transition. Production enrollment remains pending.

- The managed-root writer adapter preserves terminal poll failures
  across host and cluster-runner restarts, including cancellation after a
  complete receipt. Managed activation remains gated separately.

- Managed edge cache bypass survives supervisor restarts in its persistent
  configuration directory. Startup refuses a later cache-enabling configuration
  or damaged retained policy, and concurrent supervisors cannot own the same
  directory. Backend managed-root activation remains gated separately.

- SQLite native UPDATE acknowledgments survive their own committed generation
  change while retaining original writer, key and deadline checks. Ordinary DNS
  answers still reject stale generations. Unavailable startup captures return
  empty unsigned SERVFAIL responses without borrowing file-backed keys.

- Replication primitives retain request deadlines through peer authorization
  and coordinator queueing. Forwarded command expiry is checked at the actual
  SQLite commit/no-op boundary, including deduplication responses. Production
  enrolled HA remains pending.

### Added

- Offline `anchors status` and `anchors recover` for complete managed-root loss,
  with explicit replacement trust, retained revocations and recovery history,
  exact expected-version checks, and an audit committed with the recovery.

- Explicit direct-topology RFC 5011 activation for SQLite nodes, with an
  elected updater, independent follower denial, accepted managed publication
  and retained restart history. Follower terminal failures survive replica
  installs; successful normal observations allow a later fresh installed-source
  acquisition, and authority changes join old acquisitions. The host declaration
  rejects PROXYv2 ingress. Explicit edge topology now requires authenticated
  installed persistent cache bypass from every declared peer before startup;
  edge/backend restarts retain managed trust without fallback. Full enrolled HA
  orchestration and initial empty edge-follower installation remain separate.

- Managed edges can expose a separate authenticated TLS observation listener
  with request-bound deployment, child and membership identities. Missing or
  stale installed state refuses; backend activation remains separately gated.

- Kubernetes loads complete encrypted stored/file TSIG keys once at startup for
  native and encrypted DNS, secondary acquisition and NOTIFY. Unknown or invalid
  private sources refuse signed work without blocking unrelated unsigned saved
  DNS. Failed preparation and credential changes require restart; Kubernetes
  hot key management remains unavailable.

- SQLite TSIG key administration exposes permission-checked metadata, creation,
  replacement, overlap removal and revocation. Commands retain original writer,
  credential and source checks; receipts distinguish durable commits from
  confirmed serving publication. Secret material is never returned.

- Release benchmark collection now includes cache, upstream strategies, sockets
  and persistence in fresh processes, with retained failures and baseline
  integrity checks. Fixed-runner calibration remains required.

- Linux DoQ and HTTP/3 listeners now honor `listeners.sockets` with connection-ID
  routing across path migration and shared application limits. A pinned quic-go
  fix prevents CID exhaustion across repeated migrations.

- Kubernetes: browser login starts and callbacks now share durable request
  budgets across replicas and restart. Conflicts and admission-store outages
  refuse provider dispatch without local fallback.

- SQLite: browser login starts, callbacks and CLI preparations share durable
  request budgets across adapters and restart. Unavailable or malformed counters
  refuse before authentication; superseded provider requests are not replayed.

- SQLite: local-login, TOTP-management and self-service password-change lockouts
  share persistent counters inside the original authentication command, surviving
  restart and complete replica transfer. Failed proofs retain counters; later
  command failures roll back counters, password, enrollment and factor changes
  together. Password changes verify the factor once before changing the password.

- Kubernetes: local-password lockouts share durable IP-prefix and username
  admission across replicas and restart. Concurrent attempts reserve capacity;
  unavailable shared state refuses login without issuing a session.

- Static SQLite twins: configure named peers for authenticated status and list
  reads while each node retains local leadership. Distributed files own object
  and cluster-setting changes; object and credential mutations are refused, and
  public block-page requests are disabled. Dashboard actions reflect this mode,
  and blocking toggles report node scope.
- CLI: `cluster status` reports the connected node's store, leader, membership
  count and read-only mode, with table/wide/JSON/YAML output.
- Kubernetes: blocking toggles persist in the shared runtime ConfigMap and
  converge across replicas with the same absolute expiry after restart. Conflicts
  and unknown write outcomes are returned without replaying the change.
- Kubernetes: opt-in direct `DNSEndpoint` ingestion creates source-owned record
  sets through the elected controller, including update, deletion and restart
  handling while keeping source CRs read-only.
- Store: guarded deletes retain the original UID/version across drivers; atomic
  Kubernetes creations remain editable by their owner without taking foreign fields.

- SQLite DNS: load encrypted stored TSIG keys into complete serving snapshots
  and secondary/RPZ acquisition, retaining each request's original key through
  its response. Stored-key administration and Kubernetes rotation remain pending.

- SQLite integrations: Tier-B webhook client inventory and declared actions,
  with original observation expiry, named TLS credentials and single-attempt
  action outcomes. Hooks and remote record publication remain unmounted.

- Recursive DNS: bounded RFC 8198 NSEC/NSEC3 proof reuse answers covered
  names and wildcard questions without another authority query. Original proof
  expiry survives aliases and outer-cache storage; uncertain proofs fall back
  to ordinary resolution.

- SQLite file and inline RoleBinding removal now records durable grant history
  before publication, so regranting the same binding cannot revive old cookies
  or personal tokens, including across restart and replica state transfer.

- CLI: standalone external-dns/Tier-B webhook conformance with explicit TLS
  credentials, bounded protocol checks, opt-in mutation probes and sanitized
  reports. Uncertain effects stop without automatic retries or cleanup.

- Client identification: optional host-selected asynchronous reverse-PTR fallback
  with bounded workers/cache, strict answer validation, original TTL expiry and
  inventory/DHCP precedence.
- Node settings: durable SQLite log preferences and null resets survive restart;
  unsupported node writes are explicitly read-only in the API and dashboard.
- VIP notifications: bounded keepalived transition journal feeds audit and live
  permissioned SSE with original credential/source expiry and gap refusal.
- Edge: targeted node health qualifies same-process named backend additions;
  legacy or structural changes retain validated restart behavior.

- VIP: opt-in read-only kube-vip/MetalLB ownership evidence with bounded source
  reads, exact source identity and expiry; status never grants writer authority.

- Cache: permissioned exact-key REST inspection reports TTL/state/size metadata
  without resolving misses or exposing stored answer bodies and client subnets.

- Blocking: exact captured ClientGroup scope for allow/deny lists, including
  shared policies and legacy group-list names. General lists can declare bounded
  namespace-local `clientGroupRefs`; conflicting reserved-name scopes are refused.

- Supervisor configuration: typed host-only `edge.*` and `vip.*` settings feed
  existing render/run commands with file/environment/explicit-flag precedence.
  Remote settings writes and resets remain refused.

- Kubernetes store: explicit watch cursors resume retained server event history,
  including deletions, with detectable expiration and bounded delivery. Initial
  informer list/watch behavior is preserved.

- Query statistics: exact retained-fact domain/hour indexes remove the current-hour
  100,000-name cliff. Local schema 3 and central schema/compatibility 6 include
  atomic migration and old-janitor guards; central upgrades require write locks
  and additional index storage.

- Query log: reload admission and sinks with each request retaining its original
  publication. Accepted batches drain once to their original sink; local history
  stays readable after disabling logging or restarting with a slog sink.
- CLI: use generated OpenAPI operation methods beneath the existing auth,
  pagination, SSE and error helpers, with reproducible endpoint dispatch.

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
- Additional query-log sinks. `querylog.sinks` lists up to eight destinations
  that receive every recorded query next to the local database or log lines
  selected by `querylog.sink`, with the privacy mode and exclusions already
  applied. Each sink has its own bounded queue, batch size, flush interval,
  timeout and attempt budget, so a slow, stalled or failing destination loses
  only its own queries and never delays DNS answers, the local database or the
  live view. Changes apply without a restart: an unchanged sink keeps running,
  a new one starts with new queries only, and a removed one finishes what it
  holds. On Kubernetes, a query recorded at the very moment a sink's entry is
  changed can miss both the old and the new sink; it is counted as dropped for
  that sink and stays in the local database. Four types ship: `slog` (one
  structured log line per query), `clickhouse`, `loki` and `dnstap`. See
  `docs/deploy/query-log-sinks.md` (D371).
- ClickHouse and Grafana Loki as query-log destinations, sent by the server
  itself with no agent beside it: one `INSERT ... FORMAT JSONEachRow` per batch
  over the ClickHouse HTTP interface, or one Loki push per batch with client,
  domain, query type, status and trace ID as structured metadata and only the
  node and the status class as labels. The operator page has the ClickHouse
  table definition, the Loki mapping and example settings. A batch is sent
  again, at most `attempts` times, only when nothing was sent or the
  destination answered 408, 429 or 5xx (`Retry-After` honored up to 30
  seconds). A repeat after a 5xx can store a batch twice when the destination
  had stored it; the documented ClickHouse table and Loki drop the duplicate,
  and `attempts: 1` turns repeats off. Every other answer, and a request that
  was sent and got no answer, drops the batch. The server log names the HTTP
  status and, for ClickHouse, its error code, never the destination's error
  text (D371).
- Credentials of query-log sinks are references, never values in the
  settings: `passwordRef`, `bearerTokenRef` and `caRef` name a file or an
  environment variable that the node's own configuration allows
  (`credentials.allowedFiles`, `credentials.allowedEnvironment`), or, on
  Kubernetes, a key of a Secret in kudurru's namespace with the label
  `kudurru.io/query-log-sink: "true"`. A sink reads its credential again after
  a 401 or 403, so a rotated password or token is picked up without a restart.
  A grant is not bound to a destination: whoever may change the settings can
  have a granted credential sent to any URL, so grant only credentials made
  for these destinations.
- dnstap output. A `querylog.sinks` entry of `type: dnstap` sends every
  recorded query as dnstap messages in Frame Streams to a collector such as
  DNS-collector, Vector or the `dnstap` command, over a Unix socket, TCP or TLS
  (`caRef`, `certRef`/`keyRef` for mutual TLS). Local zone and local record
  answers are `AUTH_QUERY`/`AUTH_RESPONSE`, all others
  `CLIENT_QUERY`/`CLIENT_RESPONSE`; `forwarder_query`/`forwarder_response` add a
  summary pair for queries that went to an upstream. By default a message
  follows `querylog.privacy`, carries the question and the response code but
  never the answer records, and nothing is sent under `nothing` (D372).
- The dnstap raw override: `raw: true` sends the DNS messages exactly as
  received and answered, with the real client address and port, whatever the
  privacy mode says. Only an administrator can save it (new permission
  `querylog.raw`, held by the `admin` role); the change is audited as
  `querylog.raw`, and every node records `querylog.raw.export` before it sends
  the first raw message and does not send if it cannot record it. Excluded
  clients and domains are never sent, raw or not, and while a raw sink is
  configured a change of `querylog.exclude` also needs `querylog.raw` (D372).
- Per-sink query-log metrics: `kudurru_sink_sent_total`,
  `kudurru_sink_errors_total`, `kudurru_sink_queue_length` and
  `kudurru_sink_last_success_timestamp_seconds`, labeled by sink name, and
  per-sink loss in `kudurru_querylog_dropped_total{stage="sink",sink}`, which
  the existing `KudurruQuerylogDrops` alert covers.
- Integrations with `deletionPolicy: Delete` run on SQLite, and deleting one
  cleans up before the object goes. Such an Integration carries the reserved
  finalizer `kudurru.io/integration-cleanup`, set in the same write as the
  object. Removing its document leaves it visible with phase `Terminating`;
  the elected writer then deletes the records the instance provably published
  (every row carries its marker and the ledger lists every provider ID),
  forgets their ledger rows, removes the local state of its source in one
  commit and removes the object. `Delete` overrides a zone's `upsert-only` or
  `create-only` policy for those records; records of other owners, hand-made
  records and anything that only carries a copied marker are never touched.
  The cleanup works in bounded runs, shares the Cloudflare request budget,
  continues after a server restart and after a failover to another elected
  writer, and pauses without any provider request while the license is
  expired. A delete whose answer was lost is never sent again; the next writer
  confirms it by rereading the zone. The audit log records the purge
  (`integration.purge`) and the finalization (`integration.cleanup`) (D367).
- A cleanup that cannot finish keeps the Integration `Terminating` with a
  reason and counts in the integration status and on the object's `Ready`
  condition and `status.records`. Blocking reasons: `RecordsConflict`,
  `OwnershipUnproven`, `PendingRecovery`, `DestinationRemoved`,
  `SnapshotIncomplete`, `LocalStateRemains` and `CapacityExceeded`. Paused or
  retrying: `CredentialsUnavailable`, `ProvisionFailed`, `ProviderFailed`,
  `ProviderUnavailable`, `RateLimited`, `BudgetExhausted`, `CleanupFailed` and
  `ReleaseFailed`. A blocked cleanup is checked again at the resync interval,
  at most every ten minutes.
- `kudurru integrations release NAME --uid UID --revision REV` (and
  `POST /api/v1/integrations/{namespace}/{name}/release`) releases a
  `Terminating` Integration without finishing its cleanup: the exact reviewed
  object is removed, its provider records and ledger rows stay as under
  `Retain`, and no provider is contacted. It needs neither a license nor the
  credential, refuses a live object or a changed UID or revision with
  `409 Conflict`, needs the new `integrations.release` permission (admin
  only), and is audited as `integration.release` (D367).
- Integration summaries (`GET /api/v1/integrations`, `kudurru integrations
  get` and `list`) show `deletionPolicy`, `terminating`,
  `deletionRequestedAt` and, while a cleanup reports, its `remaining`,
  `blocked` and `uncertain` counts; `kudurru integrations list` has the new
  columns POLICY, REASON and DELETION REQUESTED.
- The follower contract of an enrolled SQLite cluster. Every changing API
  operation has one follower class, published in the OpenAPI document as
  `x-kudurru-follower` and in the reference's endpoint index as an "On a
  follower" column. An enrolled follower's `GET /api/v1/cluster` carries
  `primaryRequired: true` with `forwardedOperations` and `localOperations`;
  every other changing operation answers `503 primary_required` there, after
  the credential and permission checks, with a hint naming the primary and
  without `Retry-After`. The CLI prints a hint for `primary_required`,
  `completion_unknown`, `cluster_fenced` and `cluster_conflict` (D368).
- An enrolled follower forwards sign-out (`POST /auth/logout`,
  `POST /auth/logout/browser`), the revocation of one of your own sessions
  (`DELETE /auth/sessions/{id}`) and cluster settings changes
  (`PATCH /settings`, the dashboard's Settings page) to its primary, which
  repeats every check of its own route. A settings change to keys that changed
  on the primary since the follower installed them answers `409 conflict`; a
  committed change is answered once the follower serves it. If the primary
  cannot be reached, sign-out still clears the cookies and answers
  `503 unavailable` or `503 completion_unknown`, saying that the session may
  stay valid until it expires. Node settings (`PATCH /node/config`) stay
  primary-only (D368).
- `kudurru apply`, the dashboard's block list, client group
  and policy editors and `POST /api/v1/{kind}:batchDelete` work through an
  enrolled follower: each document or name is forwarded as its own command, in
  order, inside one 30-second request (D375).
- `POST /api/v1/cache/flush` (`kudurru cache flush`) works on an enrolled
  follower and flushes the response cache of the node that answers (D368).
- `GET /api/v1/cluster` and `kudurru cluster status` report the replication
  role of the answering SQLite node: `role` (`standalone`, `primary` or
  `follower`) and `primary`, and the CLI has a `ROLE` column. On a follower
  `leader` names the primary, and `GET /api/v1/nodes` no longer marks the
  follower as leader. `GET /api/v1/auth/status` on a follower carries
  `role: follower` and the primary's name and lists no OIDC provider.
- `kudurru iam rolebindings get` and `GET /api/v1/iam/rolebindings/{namespace}/{name}`
  work on Kubernetes and return the RoleBinding with its UID and resource
  version. RoleBinding writes through the API remain SQLite-only (D374).
- Runbook "Add a follower behind managed edges": the order for adding a SQLite
  follower behind dnsdist edges with managed root anchors (edge backends, then
  the offline join, then start).
- Operator guides for the enrolled SQLite cluster
  (`docs/deploy/sqlite-cluster.md`: preparation, activation, invitation and
  join, checks, what a follower passes on, promotion, the return of the old
  primary, recovery after two promotions, limits and the answers of the
  cluster commands) and for RFC 2136 dynamic updates on both store drivers
  (`docs/deploy/dynamic-updates.md`), a Kubernetes section in
  `docs/deploy/rolebindings.md` (the `kudurru-grant-floors` Secret), the
  runbook "The primary of an enrolled SQLite cluster is lost", the
  administrator guide's authenticator, session and identity-provider sign-in
  sections, and an index of every task page in `docs/deploy/README.md`.
- Contributor tooling: `make interop-internal` runs the BIND 9
  interoperability tests of internal packages; `KUDURRU_REQUIRE_BIND=1` and
  `KUDURRU_REQUIRE_ENVTEST=1` make a test fail instead of skip when BIND 9 or
  envtest is missing; `KUDURRU_E2E_LICENSED_BINARY` names a prebuilt
  `-tags licensetest` binary; `hack/kind-e2e.sh` accepts
  `BUILD=0 IMAGE=<repository:tag>` and refuses an unusable image before it
  creates a cluster; `make test-sinks` runs the query-log sink tests against
  real ClickHouse and Loki; and golden documents pin the byte formats of
  managed root-anchor history.

### Changed

- A zone that the SQLite image cannot serve is refused when it is written
  (Docker, systemd, the chart's `standalone` mode): a DNSZone of a type no
  release serves yet (`external`, `catalog`, `secondaryCatalog`), with a
  `queryACL` or `transferACL` entry that is not an address or a network, with
  a notify target that is not an address, a primary zone whose
  `spec.policy.update` is not `deny` or `allow`, a primary zone with an
  `updatePolicies` entry that cannot be compiled (a `name` outside the zone, a
  type that does not exist or that an update cannot carry, an empty
  `tsigKeyRef`; also when updates are denied), a zone with the `spec.zoneName`
  of another DNSZone, or a zone created under a new object name for the zone
  name of a zone deleted on this node. The API answers `400 invalid` and names
  the field; the file loader logs the object as invalid and loads the rest.
  Kubernetes is unchanged: the API server admits the object and the
  controller reports it (D373).
- An upgrade of a SQLite data directory checks what 0.1 stored before it
  changes anything. If `state.db` holds an object that 0.2 cannot serve,
  kudurru does not start: it names every such object, leaves the directory
  exactly as 0.1 wrote it and says what to do (runbook "Upgrade refused:
  stored objects are not valid in this release"). A `querylog.db` written by
  a newer release is refused with `querylog.db was written by a newer
  kudurru` and left byte for byte as it was (D373).
- The server log says why a serving generation is not published, with the
  object and the reason, once per failure (`complete serving generation not
  published`).
- `upstreams.recursion.anchorMode: managed` fails validation on the Kubernetes
  store driver, at startup, on settings reloads and in settings changes through
  the API; a refused reload keeps the last good configuration. It used to be
  accepted and run the seeds as plain static anchors. Managed root anchors are
  SQLite-only in v0.2 (D370).
- Managed root anchors on an enrolled SQLite cluster: a member that returns
  after a promotion, or that lost an offline conflict resolution, takes the
  current primary's trust-anchor state by term on its next pull, but never
  trusts a root key again that it saw revoked: while the primary has not
  recorded that revocation, the member refuses the primary's data, stays
  fenced and answers DNS from its own last-good data. "A member returns with
  different trust-anchor state" in `docs/deploy/sqlite-join.md` says how to
  recognize each refused state and what ends it (D369).
- Managed root anchors: fenced and conflicted members keep their own
  deny-only check of the root DNSKEY set (about hourly, outbound TCP 53 to the
  configured root servers), like followers; a signed revocation retires their
  DNS output until they install newer trust-anchor state. They never update or
  bootstrap trust anchors (D369).
- A changing request that an enrolled follower neither forwards nor runs
  itself answers `503 primary_required` instead of `503 unavailable` or other
  texts (allow and deny lists, identity administration, own tokens, password
  and authenticators, unblock decisions, import and migration runs and
  previews, zone NOTIFY, stored TSIG keys, OIDC sign-in, integration routes,
  list refresh, DHCPScope writes). Scripts that matched `unavailable` there see
  `primary_required`; the status stays `503` (D368).
- A member whose write authority is fenced or in conflict answers every API
  request with `503 cluster_fenced` or `503 cluster_conflict`, with a hint and
  without `Retry-After`, instead of `generation_pending` or `unavailable` with
  `Retry-After: 1`. The conflict hint names `kudurru cluster resolution
  --offline` and `kudurru cluster resolve --offline`.
- `GET /api/v1/settings/_schema` on an enrolled follower marks node settings
  `readOnly` (`PATCH /node/config` is refused there); cluster settings stay
  writable because their changes are forwarded. On a follower, and on a fenced
  or conflicted member, the block page shows "This server cannot take a
  request for access right now" in place of the unblock request form.
- `kudurru login --username` against an enrolled follower reads
  `GET /api/v1/auth/status` first and refuses with `primary_required` before
  it sends the password.
- On Kubernetes, public CLI login (`kudurru login` without `--token` or
  `--username`, and `--device`) answers `503 cli_login_unavailable` with a
  hint, and the CLI prints a message that names `--token` and `--username`;
  the provider logout continuation routes answer
  `503 provider_logout_unavailable`. Both need the complete SQLite runtime
  (D374).
- On SQLite, an unsafe request to a path under `/api/v1/zones`,
  `/api/v1/unblockrequests` or a BlockList's source entries that is not a
  route answers `404 not_found` instead of `503 current_command_unavailable`
  (D374). The settings ownership and YAML routes keep answering
  `503 unavailable` and now carry a hint naming `kudurru settings set` and
  `get`; `PATCH /api/v1/node/config` and `PATCH /api/v1/settings` declare the
  `501 read_only` answer Kubernetes already gave for node-scope changes.
- A request with several documents or names on SQLite (`kudurru apply`,
  `kudurru restore`, `POST /api/v1/apply`, batch deletion) stops when
  something else is changed on the node through the API while it runs:
  another changing request, a sign-in or a write that a follower passes on.
  Every later item reports `unavailable` ("another change was committed on
  this node while the request ran"), and the items before stay applied. A
  request that has run for two seconds waits for the node to publish and
  pauses 200 milliseconds before its next document, so long requests are
  slower (D378).
- `kudurru apply` and `kudurru restore` wait up to five minutes for the
  answer by default, as long as the server lets one request run;
  `--request-timeout` still overrides it.
- A credential file, environment variable or Secret key that holds more than
  64 KiB is refused with `credential is unavailable: it holds more than 64 KiB`
  instead of the bare `credential is unavailable`, for identity providers and
  integrations as well as for query-log sinks.
- Shutdown gives additional query-log sinks up to five seconds to write what
  they hold; a sink that did not drain is named in the warning `additional
  query-log sink did not drain`. `query log flush incomplete` keeps meaning
  that the local query log could not be flushed. The collector warns when a
  sink loses queries, at the first loss and at each tenfold total.
- A live Integration with `deletionPolicy: Delete` stored without the reserved
  finalizer is shown as `Paused` with `CleanupFinalizerMissing` and makes no
  provider call until the next load of its unchanged document adds it.
  Declaring a `Terminating` Integration with `deletionPolicy: Retain` (or
  without the line) releases it without contacting the provider, logged as
  `objects: finalized` and audited as `integration.finalize`. Check, preview,
  sync and actions are refused for a `Terminating` Integration. The objects
  loader requests the deletion of a file-managed object once. On Kubernetes no
  Integration executes yet, so `deletionPolicy` has no effect there, as the
  CRD field description now says (D367).
- `make test` exports the envtest binaries that `make envtest` installed to
  every Kubernetes test suite when `KUBEBUILDER_ASSETS` is unset; five
  packages silently skipped their Kubernetes tests without it.
- Documentation: the architecture tour (`docs/architecture/`) describes v0.2.0
  as built, with a table of what differs between the SQLite and Kubernetes
  store drivers, the leader and objects-loader placement corrected and new
  sections on follower sign-in, external sessions and static twins; the guides
  and the documentation site compare against v0.2.0; `docs/deploy/tsig-keys.md`
  describes Kubernetes hot reload; the upgrade runbook gives the order for a
  Kubernetes installation from v0.1 (stop, CRDs, offline IAM migration, Helm
  upgrade); the administrator guide explains client ID and MAC matchers,
  forward-zone DNSSEC choices and temporary validation exceptions.

### Fixed

- Zone lookup reserves space for a CNAME and its target together, avoiding an
  extra answer-slice allocation on ordinary alias queries.

- Blocking: avoid scanning indexed regex batches after a coincidental gram hit
  when every original required literal is absent, preserving rule order and
  exact fallback behavior.

- Browser login and logout can acknowledge their own committed state while
  retaining original writer, dependency, cookie-policy and expiry checks.

- Blocking: preserve regex rules with IPv4/IPv6 CIDR client modifiers instead
  of mistaking the CIDR slash for the regex delimiter and discarding the rule.

- Secondary and RPZ acquisition now retain their original job deadline through
  persistence and publication, preventing expired work from becoming fresh data.

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
- On SQLite, one run of `kudurru apply` or `kudurru restore` applies every
  document of its files, and `POST /api/v1/{kind}:batchDelete` (the
  dashboard's multi-select delete) deletes every selected object of one
  namespace. Before, a standalone node or a primary applied only the first new
  or changed document and answered `unavailable` for every later one. The
  dashboard sends a selection across several namespaces one request at a time
  (D378).
- One object could stop a whole SQLite node: a stored DNSZone the serving image
  could not build, a zone re-created under another object name, a record set
  whose `zoneRef` named a forward zone, or a block list whose URL had never
  downloaded stopped every DNS answer, readiness and the whole API, across
  restarts. Such zones are now refused at write; a record set that names a
  forward zone is stored and reported (`forward zones hold no records`), as in
  0.1; a list source whose first download has not finished, or failed, is
  served empty and the failed fetch is reported on the list (D373).
- A record set the zone cannot hold no longer freezes its zone on SQLite: an
  endpoint that does not parse, two endpoints for one name and type, a CNAME
  next to other data, a CNAME or NS at the apex, data below a DNAME, or two
  DNAME targets for one name with different set identifiers is left out and
  reported on its record set, as in 0.1, and the rest of the zone is served.
  Of two such DNAME targets the first is kept. A record set that becomes
  invalid as a whole is left out instead of keeping its earlier records (D373).
- A zone serial no longer changes on the first start after an upgrade from 0.1
  on SQLite, unless a name holds two DNAME targets, or two CNAME targets of
  which the dropped one has the lower TTL: only the first target is kept, with
  its own TTL, so the served content changes and the serial advances (D373).
- Managed root anchors: a SQLite member that is fenced or holds an unresolved
  promotion conflict keeps answering DNS from its last valid local data and
  last accepted trust anchors, and restarts in that state, with
  `managedTopology` `direct` and `edges`; it answered `SERVFAIL` for every query
  and refused to start. Every follower passes through the fenced state at each
  promotion, so this also removes that outage window (D369).
- Managed root anchors: a member whose clock is up to five minutes behind the
  primary no longer loses its revocation checks, and a newly promoted primary
  no longer loses trust-anchor updates, for the rest of the term; the member
  waits for its own clock and logs `check time synchronization`. A larger gap
  is still refused with `invalid clock continuity` (D369).
- Managed root anchors: a returning member whose trust-anchor state had been
  updated as often as the primary's, or more often, takes the primary's state
  on its next pull instead of staying fenced until the primary's state had
  been updated more times (D369).
- A node with no installed cluster image (an unfinished `cluster join`)
  started with `managedTopology: direct` or `edges` fails with an error that
  names `kudurru cluster join --offline` and `kudurru cluster join-reconcile
  --offline`, instead of "root anchor policy unavailable or changed" or a raw
  store error (D370).
- On Kubernetes, six API routes that the server forwards to its authentication
  stack answered a plain-text `404 page not found` instead of the JSON error
  envelope: the RoleBinding item read, the three `/auth/cli` routes and the two
  `/auth/oidc/{idp}/logout` routes (D374).
- A write sent through an enrolled follower could answer
  `503 completion_unknown` although the primary had committed it, when another
  write on the primary landed between the commit and the answer; the primary
  now answers a forwarded command before any other write can proceed (D376).
  A follower that cannot bind a request for forwarding now says why (data
  changed, lifetime ended, canceled, route not forwarded) instead of always
  "the follower's source changed" or `401 unauthenticated` (D375).
- Signing out on an enrolled follower answered `503` and left both the cookie
  and the durable session valid (D368).
- A query whose upstream fails or times out is answered with SERVFAIL also
  when a configuration change is committed around it; the node used to send
  no answer, so the client waited for its own timeout. The same release also
  covers empty SERVFAILs that the node produces itself with those statuses, so
  on a node whose default upstream group is recursive a cache-miss query in
  the tens of milliseconds after a commit is now answered SERVFAIL (extended
  error 23) where it used to get no answer and succeed on retry (D377).
- `Delete` cleanup: a long record-change history is forgotten in commits sized
  to the 30-second run; a run that a slow store keeps from finishing is
  reported as `CleanupFailed`, not `ProviderFailed`; a failed first count keeps
  the counts last reported; a provider delete cut before its receipt is
  confirmed by a reread and recorded in UTC (D367).
- Query-log HTTP sinks read the answer of a destination that refuses a large
  batch before reading it and resets the connection, and believe a 2xx only
  for a body that was written whole.
- The release workflow installs the envtest binaries before its test job; the
  primary-zone runtime tests fail without them.
- The dashboard no longer says that MAC address, host name and name pattern
  matchers "never match" or "arrive in v0.2", and the settings help and the
  RoleBinding CRD reference no longer promise `kudurru login` or zone groups
  for v0.2 (the `zonegroup` scope is accepted but reserved and grants
  nothing). The Authenticator panel says "Authenticator management is
  unavailable on this server" on Kubernetes.
- Documentation: eleven sequence diagrams of the architecture tour did not
  render; `docs/deploy/kustomize.md` named the wrong tree for v0.1.0; the
  migration guides said that `kudurru query` shows an allow-list decision; the
  administrator guide called the Identity tab read-only; and several pages
  still called enrolled replication, Kubernetes TSIG rotation or durable
  Kubernetes revocation open.

Stub zones and signed secondary serving are implemented on the development
branch. Inbound NOTIFY verifies the primary source
and configured TSIG, schedules bounded local refreshes, and fans authenticated
hints to peers so every replica transfers independently.

### Security

- Upgrade `golang.org/x/net` to v0.62.0 in the product and isolated AD
  protocol spike to address CVE-2026-78661, together with its required Go
  dependency versions.

### Upgrade notes

- SQLite (verified with the released 0.1.0 binary): back up first. An upgrade
  cannot be undone: `state.db` goes from schema 1 to 9, `querylog.db` from 2 to
  3 and the `local.db` marker from 1 to 9, and the 0.1.0 binary then refuses
  the directory (`state.db was written by a newer kudurru`) without changing
  it. In the directories the upgrade tests use, answers, zone serials (except
  for the CNAME and DNAME shapes in the next note), object UIDs and
  generations, API tokens, the admin password, the query log and the audit
  trail carry over. A setting saved from the 0.1 Settings page is
  effective after the upgrade; from then on cluster settings live in
  `state.db` and the settings file no longer overrides them. The response
  cache starts empty. A secondary or stub zone that 0.1 stored with its
  configuration is now served. A primary zone with a child zone can answer
  `SERVFAIL`, and readiness can stay 503, for up to about a minute on the
  first start. For a moment after readiness turns 200 an API write can be
  answered `503 unavailable` or `generation_pending` without effect;
  automation should repeat such a request.
- SQLite: a name that 0.1 served with two CNAME targets (different set
  identifiers, or sources merged under `MergeUnion`) still answers with the
  first target, but now with that target's own TTL instead of the lower TTL
  of the two; when that changes the TTL, the zone gets a new serial on the
  first start. A name that 0.1 served with two DNAME targets now answers with
  the first one only, and the zone gets a new serial. Give each such name one
  target before the upgrade to keep the zone unchanged.
- Kubernetes (verified with the 0.1.0 binary built from the tag on envtest):
  the CRDs stay `kudurru.io/v1alpha1` with no conversion; apply the CRD chart
  (or `deploy/kustomize/crd`) first. Objects 0.1.0 stored keep their UID,
  generation, spec and status; objects the tightened schema refuses stay
  stored and do not stop the node, and a change to them is refused until they
  are corrected. When the namespace holds 0.1 IAM Secrets or AuditLog batches
  the upgrade is offline: stop every process, apply the CRDs, run
  `kudurru iam migrate status`, `plan` and `begin --offline`
  (`docs/deploy/iam-upgrade.md`), write the audit key ring with
  `kudurru audit keys init --legacy-master-file ... --legacy-before ...`,
  publish it as `kudurru-audit-keys`, and start with
  `store.kube.expectedClusterID` set. The first break-glass token 0.1 minted
  almost always does not survive the migration; mint a new one with
  `kudurru admin create-token`. `helm upgrade` from the 0.1.0 chart keeps
  every field Kubernetes does not let an upgrade change.

### Known limitations

- On SQLite, while the documents of a long multi-document request commit,
  the node can leave its own zones unanswered and refuse other API callers
  with `503 generation_pending`. The pace described under Changed is meant to
  bound this to about two seconds at a time; under load a 120-document apply
  has shown more than a minute without a local DNS answer.
- Through an enrolled follower, a file that does not fit between two of the
  follower's pulls (five seconds by default) or inside the 30-second request
  is cut: later documents report `unavailable` and are not applied, earlier
  ones stay committed, and repeating the command through the follower does
  not finish it. Send such a file, and every restore, to the primary (D375).
- A write through a follower whose exchange with the primary fails, including
  an unreachable primary, answers `503 completion_unknown`: inspect the object
  before a new decision.
- CLI password login (`kudurru login --username`) is refused on an enrolled
  follower; save a context with `--token-stdin` for a follower. OIDC, browser
  and device sign-in run on the primary.
- An enrolled cluster has no command that removes a member: a replaced member
  stays listed, keeps its name and internal address reserved and counts
  toward 64 active members, so a replacement needs a new name and address.
- Managed root anchors: when the primary can never record a root-key
  revocation that one member saw, that member never follows the primary
  again; it keeps answering DNS from its own last-good data and must be
  replaced. No command records a revocation on the primary by operator
  decision.
- Integrations: `deletionPolicy` has no effect on Kubernetes, where no
  Integration executes yet. On SQLite, above 100,000 stored objects or
  256 MiB of stored documents no Integration job runs, a `Delete` cleanup
  included.
- An unpinned Kubernetes node started while the API server is unreachable
  exits before it answers; set `store.kube.expectedClusterID` so that it can
  serve from `local.db`.
- Managed root anchors behind managed edges: adding an empty follower behind
  dnsdist edges is tested in process only, not against a real dnsdist.

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
