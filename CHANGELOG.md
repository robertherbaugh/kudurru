# Changelog

Notable changes to kudurru, following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/). Until v1.0, the
`kudurru.io/v1alpha1` API may change between minor releases.

## [Unreleased]

## [0.2.0] - unreleased

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

- An enrolled SQLite follower forwards a TSIG-signed RFC 2136 UPDATE to its
  primary. The follower checks the zone and the signature; the primary applies
  its own update policy and prerequisites and commits once; the follower
  returns the primary's answer (`NOERROR`, `NXRRSET`, `REFUSED` and so on),
  signed with the request's key, so update clients can use any member. The
  key must have the same name, algorithm and secret on every node, and member
  clocks must agree within a second (a forwarded update expires at most four
  seconds after the follower sends it). A follower that cannot reach its
  primary, or loses the connection after sending, answers `SERVFAIL` and never
  retries; fenced, conflicted and static nodes answer `SERVFAIL` without
  forwarding. A follower answers `REFUSED` for an unsigned UPDATE and
  `NOTAUTH` for a zone it does not serve as a primary zone, as the primary
  does (D381).
- An enrolled SQLite follower forwards your own password change
  (`POST /auth/password`) and the creation and revocation of personal API
  tokens (`POST /auth/tokens`, `DELETE /auth/tokens/{id}`) to its primary, so
  an account that must change its password can do so on the follower it
  signed in to. The new password and a new token's secret never travel to the
  primary: the follower sends only their hashes, and the primary applies the
  shared lockout counters and every check of its own route. A committed
  password change is never reported as `unavailable`: if the follower cannot
  give a browser its new cookie in time it answers
  `503 committed_output_unavailable`, which means that the password changed
  (D368).
- `GET /api/v1/querylog/sinks` (permission `querylog.read`) shows the
  additional query-log sinks the answering node runs: queue length and bound,
  queries sent and dropped, failed attempts, the time of the last successful
  write, and whether a dnstap sink sends raw queries. A sink still draining
  after a change is listed too. Each node reports only its own sinks.
- dnstap listen. `listeners.dnstap` accepts dnstap in Frame Streams from a
  front end such as dnsdist or Unbound over a Unix socket, TCP (only from the
  addresses in `allowFrom`) or TLS (optionally only with a client certificate
  from `clientCAFile`), and records the client queries the messages describe
  in this node's query log, through the same privacy mode and exclusions as
  every other query. A query and its response are one row; a message about a
  query this node answered itself is recognized by the client address and
  port, the question and the time, and dropped, so each client query is one
  row. Forwarder and resolver messages are counted, not recorded;
  `identities` limits the accepted senders, `ignoreClients` drops messages
  about given clients, and `unforwardedIdentities` names front ends, such as a
  managed edge, that report only the answers they give themselves. A query
  received from dnstap is never sent by a dnstap sink, and messages with the
  node's own identity are dropped, so a sink and a listener cannot loop. The
  listener never slows a sender or DNS: what does not fit, frames above
  192 KiB and messages that do not decode are dropped and counted
  (`kudurru_querylog_dnstap_frames_total`,
  `kudurru_querylog_dnstap_connections_total`); a failed accept is retried and
  logged at most once a minute; each message time more than 5 minutes off
  this node's clock is replaced by the receive time and counted in
  `kudurru_querylog_dnstap_clock_skew_total`. It is a host setting and takes
  effect at restart (D372).
- `kudurru edge run --dnstap-target tcp:ip:port|unix:/path` (settings
  `edge.dnstapTarget` and `edge.dnstapIdentity`) makes the managed dnsdist send
  dnstap of the answers it gives without a backend, from its packet cache or
  its own rules, so that they appear in the query log too.
- OpenTelemetry tracing of DNS queries (`tracing.*`, off by default): every
  stage and every upstream attempt becomes a span, and kept traces are sent to
  an OTLP/HTTP collector. A trace is kept when it is sampled
  (`tracing.sampleRate`, 0.1 % by default), when it continues an edge's trace,
  when resolution failed, or when it took at least `tracing.slowThreshold`
  (250 ms); traces kept only because they failed or were slow are capped at
  100 in any one second. An unreachable or slow collector never delays
  answers. With tracing on, a query whose trace is not kept costs two
  allocations (about 5 KB); with tracing off a query allocates as before
  (D382).
- dnsdist TRACEPARENT: a query from an authenticated edge (PROXYv2 with the
  shared secret) continues the edge's trace from its EDNS TRACEPARENT option
  (code 65500), so dnsdist and kudurru spans form one trace; the option is
  removed from every query and ignored from any other sender.
  `upstreams.groups.<name>.tracePropagation` passes sampled traces on to
  servers you run. Kept traces link from the query log (`trace_id` in the
  Queries API, the live tail and every sink) and from the
  `kudurru_dns_request_duration_seconds` histogram as exemplars. Metrics
  `kudurru_tracing_traceparent_total`, `kudurru_tracing_traces_total` and
  `kudurru_tracing_queue_length` (D382).
- Tier-B webhook hooks. A peer that advertises `hooks` and is given a
  separate `hmacKey` credential (32 to 4096 bytes) receives signed
  `POST /events` deliveries for `record.changed`, `zone.changed` (once per
  committed serial), `blocking.toggled`, `integration.error` and
  `cluster.leader_changed`. Events are at-least-once with a stable
  `X-Kudurru-Delivery` ID, survive restarts and failover through a durable
  per-instance cursor, and are retried with backoff for up to 24 hours;
  dropped events set the Integration Degraded (`EventDeadLettered`). Each hook
  instance keeps at most 384 delivery records and removes older versions of a
  subject first; every running Integration removes records an hour after
  their 24-hour deadline. Every hook instance receives events for all
  namespaces, so creating one is a cluster-level privilege (D379).
- Tier-B webhook Integrations publish records: set `spec.webhook.domains`
  (and optionally `policy`) to publish the selected records of those domains
  to an external-dns webhook provider or another Kudurru's external-dns
  listener. Ownership is kept at the peer in a TXT registry
  (`_kudurru-<type>.<name>`); records of other owners and unowned records are
  never changed, and every change is confirmed by rereading the peer, never by
  its 204. Uncertain changes stay pending and are settled from a reread on the
  next sync, also after a restart or failover. A desired record at a name
  another Kudurru's registry row still claims is reported as a conflict, and
  such a claim holds the whole name. A record set published before `domains`
  was narrowed stays the instance's own (`policy: sync` deletes it); removing
  every domain ends publication and leaves the published record sets at the
  peer, and `deletionPolicy: Delete` removes the record sets its registry
  rows name once the peer is reachable (D380).
- `kudurru integrations webhook conformance --expect-endpoints-file` rereads
  the peer's record list and checks that given endpoints are present or
  absent, for provider authors; record lists of up to 32,768 endpoints are
  accepted. Reviewed `integrations sync` and `integrations act` now record one
  audit entry each (`integration.sync`, `integration.act`), also when the
  outcome is uncertain.
- Integrations on Kubernetes (D384). Every replica runs the integration host
  and one Lease per Integration elects the replica that acts, which rechecks
  its Lease and the Integration by live reads before every provider request
  and ledger write; private state lives in Secrets in the release namespace,
  each written by one exact compare-and-swap. Cloudflare publication, UniFi
  inventory and VLAN/note groups (one committed source head per Integration,
  watched by every replica and kept during an API outage only until each
  observation's expiry), the `/api/v1/integrations` administration API and
  the `kudurru integrations` list, check, actions and observe sync commands,
  and `deletionPolicy: Delete` (the cleanup finalizer is added before the
  first provider call) work there. Commands run only on the replica that holds
  the Lease; any other replica answers 503 with that pod's name in
  `hint.holder` and does not forward. Only Integrations in kudurru's release
  namespace run; one elsewhere reports `NamespaceNotAllowed` and gets no
  credential read. UniFi Integrations that publish DNS records are refused
  (`RecordPublicationUnsupportedOnKubernetes`), and webhook Integrations
  (`UnsupportedOnKubernetes`). While a published record set is reported
  `Duplicated`, `Occluded` or `Invalid`, every Cloudflare Integration shows
  `Ready=False` with `SourceConflicted` and resumes on its own once fixed. A
  refusal whose status write keeps failing retries after one poll interval,
  doubling up to five minutes. Native Secret credential references stay
  disabled on both stores; mount the Secret as a file and allowlist its path.
- Zone groups for role bindings: label a zone `kudurru.io/zonegroup: <group>`
  and a RoleBinding (or `rbac.bindings` entry) with scope
  `{kind: zonegroup, name: <group>}` grants its role on every zone of the
  group and their record sets, on SQLite and Kubernetes. Zone lists show only
  member zones, `GET /zones` reports each zone's `zoneGroup`, `GET /auth/me`
  reports `scopedPermissions` for every zone or zone-group grant, and API
  tokens may be scoped to a zone group. Removing the label takes the access
  away at once. Changing a zone's group needs `zones.write` at cluster scope;
  on Kubernetes such a write takes the zone object over from kubectl or a
  GitOps tool (D385).
- Opt-in Kubernetes impersonation: with `auth.kubernetes.mode: impersonate`
  (Helm value `auth.kubernetes.mode`, Kubernetes driver only), every API write
  of a kudurru.io object is also sent to the apiserver as the signed-in
  principal (`kudurru:user:<name>`, `kudurru:serviceaccount:<namespace>:<name>`
  or the Kubernetes ServiceAccount, with `kudurru:group:<group>` for local
  users and OIDC bearer JWTs; OIDC users on dashboard sessions or personal
  tokens carry no groups, so bind them by user name). Kubernetes RBAC must
  then allow the write too; a refusal answers 403 with the apiserver's
  reason. The chart grants the `impersonate` verb only in this mode, and that
  grant makes a compromised kudurru pod cluster-admin-equivalent; the
  kustomize bases do not grant it. `GET /auth/status` reports
  `capabilities.factorManagement` and `capabilities.localUserCommands`
  (D386, D387).
- Operator edge blocks: drop one client address or prefix at every dnsdist
  edge until an expiry (one minute to seven days) with `kudurru edge block`,
  `kudurru edge blocks` and `kudurru edge unblock` or
  `POST|GET /api/v1/edge/blocks` and `DELETE /api/v1/edge/blocks/{id}`
  (permission `edgeblocks`; operator and admin, auditors read). Every create
  and revoke is audited; IPv4 prefixes must be /16 or longer and IPv6 /32 or
  longer; at most 256 are active. Edge supervisors pull the blocks from every
  backend's internal listener, apply them with dnsdist's dynamic-block table,
  keep them across restarts until they expire, and remember the newest
  revision of each owner, so a lagging replica cannot bring back a revoked
  block. `/status` gains an `edgeBlocks` section, with the metrics
  `kudurru_edge_blocks` and `kudurru_edge_block_applies_total` (D388).
- The dnsdist edge passes client IDs to kudurru: with `edge.clientIDDomain`
  (`--client-id-domain`) a DoT or DoQ client connecting to
  `phone.dot.example.com`, and with `edge.clientIDs` (`--client-ids`) a DoH or
  DoH3 client using `/dns-query/phone`, selects the same client group and
  policy as directly against kudurru. The ID travels in the
  secret-authenticated PROXYv2 TLV 0xE1, so the edge requires
  `proxyProtocolSecretFile`; queries with an ID skip the edge packet cache
  (D389).
- RPZ feeds on the Kubernetes store driver. Every replica transfers RPZ
  BlockList sources (signed with stored TSIG keys or unsigned), keeps an
  accepted copy in `local.db`, and enforces QNAME, CLIENT-IP and RESPONSE-IP
  triggers, logging `blocked_rpz`. A selected feed that is missing, expired or
  bound to a rotated key answers SERVFAIL (Extended DNS Error "Not Ready"),
  and so does every selected feed while the node serves a `local.db` snapshot
  older than `store.kube.snapshot.maxAge`. A feed applies from a just-started
  node's first answer, before its list is compiled. A restart while the
  apiserver is unreachable reuses the persisted copy of an unsigned feed; a
  signed feed fails closed until the pod restarts with the apiserver
  reachable. With `blocking.allowDegraded: false` a node that starts from its
  `local.db` snapshot reports ready only once that snapshot is published and
  every referenced list is in the policy (D390).
- RPZ feed diagnostics on both drivers: `kudurru lists rpz [name]`,
  `GET /api/v1/blocklists/_rpz` and `GET /api/v1/blocklists/{namespace}/{name}/rpz`
  show each feed's state on the answering node (pending, serving, expired or
  unavailable), serial, primary, last confirmation, expiry, next attempt, last
  error, rule counts by trigger and action, and every ignored rule reason with
  its count and example owners. `kudurru doctor` and `/info/messages` report
  feeds that are not serving cleanly or ignore rules, and `/metrics` adds the
  `kudurru_rpz_feed_*` series. The new guide `docs/deploy/rpz.md` lists the
  supported triggers and actions and the evaluation precedence (D391).
- `kudurru import coredns --file Corefile` previews a CoreDNS Corefile
  (CoreDNS 1.12 syntax) offline and, with `--apply`, imports it through the
  tracked migration: forwarders become upstream groups and conditional
  routes, inline hosts entries and exact name rewrites become `Policy/default`
  rewrites, and the root block's cache and access rules become cache and
  control settings (`cache.minTTL` never above `cache.maxTTL`). Like CoreDNS it
  applies a server block's hosts, rewrite and PTR answers only to names the
  longest-matching block receives, and it reports every plugin it does not
  convert, `file` and `auto` zones (for `kudurru import bind`), and blocks on
  other ports or with keys it cannot read; it never reads referenced files.
  Migration guide "From CoreDNS" (D392).
- `kudurru tsigkeys list|create|replace|reseal|revoke` manages stored TSIG
  keys on complete SQLite and Kubernetes nodes. The secret comes only from
  `--secret-file` (or `-` for stdin) and is never printed; replace, reseal and
  revoke take the exact `--uid` and `--resource-version` from `list`; input
  and output format are checked before anything is sent; a receipt that does
  not name the requested key and reviewed binding is reported as an unknown
  outcome; each command is sent once (D394).
- Managed root anchors: `GET /cluster/anchors` (`kudurru cluster anchors`,
  permission `cluster.read`) shows a node's root trust-anchor state (every
  root key with its DS key tag, RFC 5011 status, hold-down and revocation
  time, quarantine, recovery records and next refresh; this member's own
  denials; the state of the elected updater and the deny-only check; on a
  member that pulls, whether and why it refuses the primary's trust-anchor
  state, and since when) with fixed warnings, and changes nothing. The
  metrics `kudurru_root_anchor_worker_state`,
  `kudurru_root_anchor_adoption_refused` and
  `kudurru_root_anchor_admission_current` show the same on a fenced or
  conflicted member, whose API is closed, and `kudurru doctor` checks managed
  anchors on a node that runs them, including a joined member that takes
  `anchorMode: managed` from the primary (D369).
- `kudurru cluster resolve --offline --abandon-term <epoch>`: a member that
  was fenced by a newer primary but never took that primary's data
  (`"reason":"fenced_behind_term"` in `cluster resolution --offline`) can be
  kept when that primary and every member that followed it are lost for
  good. It becomes the primary of a new term on its own data, keeps every
  root-key revocation it holds, and the audit event names the abandoned term
  (D369).
- Dashboard on an enrolled SQLite follower: a banner on every page names the
  primary and says which changes are sent there; the Nodes page shows the
  node's role and the primary; controls whose change only the primary carries
  out are disabled with a tooltip that names the primary; the login page says
  that single sign-on runs on the primary.
- `docs/deploy/capabilities.md` shows what works on each deployment shape
  (standalone, static, enrolled primary and follower, fenced or conflicted,
  Kubernetes), with a glossary of every refusal key that names a deployment
  limit.
- Contributor tooling: interoperability tests with Knot DNS 3.6.2 as the
  primary of a kudurru secondary zone (AXFR, journal IXFR across a serial wrap,
  AXFR fallback, TSIG rejection, TLS with certificate verification, expiry);
  `hack/test-dnsdist-live.sh` runs the live dnsdist suites on the upstream
  dnsdist 2.1.3 image pinned by digest; `internal/rbac/permissions.yaml` lists
  declared permissions that nothing checks yet under `reserved:` with their
  target release, and a reverse lint fails the build for any other unchecked
  permission (D383, D393).

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

- Dashboard: the Nodes page of a SQLite node has a **Cluster membership** panel
  with three reviewed wizards: **Activate cluster** (a dormant standalone node
  becomes the first primary), **Invite a member** (the primary issues a one-time
  invitation for one name and internal address, shown exactly once with the
  `kudurru cluster join --offline` command and the Docker Compose changes for
  the new node) and **Review promotion** (a follower's eligibility, then a
  promotion confirmed by typing the node name). Each wizard sends exactly the
  review the operator confirmed (reading again or unticking requires a new
  confirmation, also when the stepper jumps to the last step), cannot be closed
  while its command is in flight, and sends its command once and never repeats
  it after a changed review or an unknown outcome. An invitation is valid for 2,
  5 or 9 minutes, a minute below the primary's ten-minute maximum, and a 409
  refusal names its possible causes (a changed primary, a member name or address
  already used, the primary's clock).
- Dashboard: a **Root trust anchors** panel on the Nodes page shows this node's
  managed root trust-anchor state (`GET /cluster/anchors`) and, for each state
  that needs an operator (a refused adoption of the primary's state, a member
  fenced behind its term, quarantine, a retired or terminal member, a stopped
  updater), the documented action with a link to it.
- Dashboard: **Block at edge** on every address row of **Clients** and on a
  client's page blocks the client at every dnsdist edge for a chosen time (one
  minute to seven days) with an optional reason. **Edge blocks** under the
  client list shows the active blocks with their expiry, reason and author, and
  **Revoke** lifts one early. The actions need `edgeblocks.write` and are
  disabled on an enrolled follower. When the outcome of a block or revoke is
  unknown, the dashboard says so and does not send the command again.
- Dashboard: **Query-log sinks** on **Nodes** shows each `querylog.sinks`
  destination of the node: its type, queue and bound, accepted and dropped
  queries, failed attempts and last success. Raw dnstap sinks are marked. The
  panel never shows a sink address, credential or error text.
- Dashboard: **New role binding** on **Access > Identity** creates a RoleBinding
  on the SQLite driver. Its scope picker offers the whole cluster, one zone or a
  zone group. When the server cannot say whether the binding was created, the
  dialog says so and does not send it again. API tokens can also be limited to a
  zone group.
- Integrations can be created, replaced and deleted through the API and CLI on
  both SQLite and Kubernetes: `Integration` is now a kind of
  `/api/v1/objects/integrations`, `POST /api/v1/apply` and `POST
  /api/v1/integrations:batchDelete`, and `kudurru get|apply|delete integrations`
  work. Writes bind the exact `resourceVersion` (`If-Match`), refuse objects
  owned by files or other managers, and on SQLite commit with their audit row in
  one current command; an enrolled follower forwards them to its primary.
- `GET /api/v1/integrations/manifests` and `kudurru integrations manifests` list
  the provider types of the build with their configuration schema, credential
  keys (and which are required), and what this node's integration host refuses
  (on Kubernetes: webhook instances, `spec.unifi.inventory`, Integrations
  outside the release namespace; UniFi push fields on both drivers), whether the
  license covers integrations, and which credential reference forms this node
  resolves.
- Documentation: a "Managed root anchors (RFC 5011)" page
  (`docs/deploy/managed-root-anchors.md`) that covers requirements, seed choice,
  the enable order for standalone nodes, enrolled clusters and dnsdist edges,
  monitoring, and the places where kudurru departs from RFC 5011 (permanent
  revocation tombstones, hold-down credit only within one process, seeds fixed
  after the first bootstrap).
- Managed root anchors are qualified against real authorities. Separate kudurru
  processes bootstrap from BIND 9 roots (RSA/SHA-256, ECDSA P-256, Ed25519) and
  from a Knot DNS root, and answer AD-validated recursion. They refuse wrong,
  revoked and expired trust, and they observe a real revocation. The RFC 5011
  timeline is replayed on BIND- and Knot-signed wires. An opt-in check
  (`KUDURRU_LIVE_DNS=1 go test -run TestLiveRootManagedBootstrap ./cmd/kudurru`)
  runs the production acquisition against all 13 root servers.
- Dashboard: a block list with RPZ feeds shows an "RPZ feeds on this node"
  section on its page. For each feed it shows the state the answering node
  reports (pending, serving, expired, unavailable), a plain SERVFAIL warning for
  every state but serving (pending included), the reason and message of the
  node's diagnosis, the next transfer attempt, serial, primary, last
  confirmation, expiry, last error, the compiled rules by trigger and action,
  and a table of ignored reasons with counts and example owners. It names the
  node, rereads the state after every list or source change and every few
  seconds while a feed is not serving (every 30 seconds otherwise), and flags a
  feed that answers SERVFAIL on this node beside the list's shared health badge.
- Dashboard: the Lists page shows the worst RPZ feed state of each block list
  with feeds under its health.
- Dashboard: RPZ feed sources can be added with **Add source → RPZ feed** on a
  block list's page and with **Add an RPZ feed** in the new-list wizard (policy
  zone, primaries in order, TCP or TLS, optional TSIG key name).
- `hack/release-local-checks.sh` runs the release checks that no pull request
  workflow runs (version check and its unit tests, the candidate tag,
  `make check-generated`, `make release-manifests` with an image-tag
  inspection, the real dashboard bundle with `make size`, `goreleaser check`,
  the single-target and full goreleaser snapshots and, with `--live`, the
  live-internet checks) on a clean tree, with one log per step and the commit
  it ran on, and restores the tree afterwards. With `--full-snapshot` it checks
  that both snapshot images print the snapshot version and removes them
  afterwards. Nothing is published, signed, tagged or pushed.
- `hack/live-recursive-node.sh` starts a throwaway node with a recursive
  default group and checks against the real root servers that validated
  answers carry the AD flag, a bogus zone answers SERVFAIL with EDE 6 (and its
  data with `+cd`), and recursor timeouts stay low.
- `TestLiveIANATrustAnchors` (with `KUDURRU_LIVE_DNS=1`) compares the built-in
  root trust anchors with the DS records IANA's root-anchors.xml lists as valid
  today.
- IAM writers on the SQLite driver: change a local account's display name,
  groups or disabled state and delete it (`kudurru iam users update|delete`,
  `PATCH`/`DELETE /api/v1/iam/users/{name}`); create, replace and delete kudurru
  service accounts and issue their API tokens (`kudurru iam serviceaccounts`,
  `/api/v1/iam/serviceaccounts`); create, replace and delete IdentityProvider
  objects (`kudurru iam identityproviders`, `/api/v1/iam/identityproviders`).
  Every change needs the exact reviewed UID and resourceVersion and is never
  retried.
- `kudurru iam users create` and `kudurru iam users reset-password` for the
  existing local-user API commands; passwords are read from a prompt or stdin.
- `GET /api/v1/iam/serviceaccounts/{namespace}/{name}` and `GET
  /api/v1/iam/identityproviders/{name}` on both drivers, and
  `capabilities.iamObjectCommands` in `GET /api/v1/auth/status`.
- UniFi optional writes, each off by default and run only as an explicit
  reviewed command (SQLite): `push.mirrorRecords` mirrors selected A and CNAME
  records into the site's DNS policies through a preview and a digest-bound
  `integrations sync --mode publish`, changes only policies the Integration
  itself created, never touches operator policies and reports drift; polling
  never writes.
- UniFi `dhcp-dns-servers` action (`push.dhcpDNSServers` with `push.dnsServers`
  and `push.networks`): preview the DHCP DNS server override per network, then
  apply it once with the preview digest.
- UniFi `block-client` and `unblock-client` actions (`push.clientActions`,
  requires `legacy: true`), each one legacy station command.
- `integrations act` returns an acknowledged action's answer in `result`, and
  the action's audit row records the SHA-256 of its canonical parameters (the
  same request with other whitespace or key order gives the same digest).
- Kubernetes external zones: a DNSZone of type `external` is filled from
  Ingresses, Gateway API HTTPRoutes, GRPCRoutes and TLSRoutes (through Gateways
  that accepted the route) and LoadBalancer Services, as its `spec.external`
  selects. Turn it on with the host setting `externalZones.enabled` (Helm value
  `externalZones.enabled`). The elected replica keeps one source-owned
  DNSRecordSet per source and zone, honors the external-dns `hostname`, `target`
  and `ttl` annotations (and their `alpha` forms), removes records when their
  source goes away and never changes the source objects. No license key is
  needed.
- An external zone on a server without `externalZones.enabled` is reported
  `Degraded` with reason `SourcesNotRead`, so an empty zone is no longer
  silently `Ready`.
- Helm: `externalZones.enabled` grants the ClusterRole read access to
  `ingresses` and the Gateway API `gateways`, `httproutes`, `grpcroutes` and
  `tlsroutes` only when it is set.
- Dashboard: an Integrations page (System → Integrations) lists every
  Integration with the state the host reports for its stored revision, its
  deletion policy, last sync and manager, and links to a page per instance.
- Dashboard: "New integration" is a wizard built from the node's provider
  manifests: the provider types of the build (a type the node does not run, such
  as a webhook on Kubernetes, is shown with the reason and cannot be chosen),
  the provider's settings from its configuration schema without the fields the
  node refuses (an optional group such as a Cloudflare zone's `adoptFrom` or the
  UniFi `inventory` is required to be complete only when one of its fields is
  set), and one credential reference (a granted file or environment name, never
  a value) per credential the provider reads.
- Dashboard: an instance's page runs Check, Preview and "Publish this plan" (a
  sync bound to the reviewed plan's digest), "Sync now" for inventory providers
  and the provider's advertised actions; an expired or stale preview is shown as
  refused with nothing published, a preview made before the instance changed is
  withdrawn with a "Preview superseded" notice, and after a sync the provider
  did not acknowledge the page shows the outcome as unknown and offers no
  further sync of that instance until a Check or a Preview succeeds (the open
  page holds this; a reload does not).
- Dashboard: an instance's configuration is edited in place (bound to the stored
  revision), deleted with a confirmation that explains its deletion policy, and
  while terminating shows what is left, what blocks and what is uncertain, with
  "Release without cleanup" for `integrations.release`.
- `GET /api/v1/inventory` and `kudurru inventory` list every device the client
  inventory knows, including devices that never sent a DNS query:
  inventory-integration devices (UniFi, webhook) and the answering node's DHCP
  leases, with MACs, names, tags, expiry, a `stale` flag and the state of each
  address (assigned, expired, ambiguous, superseded, withdrawn). Filters:
  source, kind, stale and a text search; keyset pages. Requires the new
  `inventory.read` permission (auditor, operator and admin roles).
- The built-in DHCP server now supplies the MAC address of a leased client, so
  `ClientGroup` `mac` matchers work without an inventory integration, in scopes
  with or without DNS registration, on both the SQLite and Kubernetes store
  drivers. An inventory integration's MAC for the same address takes precedence.
  On a SQLite cluster the lease MAC applies only to queries answered by the node
  whose DHCP server holds the lease; on Kubernetes every replica applies it.
- Dashboard: the Identity tab of the Access page creates, edits, disables,
  enables, resets the password of and deletes local accounts, creates, edits and
  deletes service accounts and issues their tokens (the token is shown once and
  forgotten when the dialog closes), creates, edits and deletes identity
  providers with a client secret reference (never a value), and edits and
  deletes role bindings (an edit keeps the binding's labels and annotations; a
  binding with more than one subject or a scope the form does not offer is
  changed with `kudurru iam rolebindings replace`). Every change names the exact
  version you reviewed and is sent once; a refusal or an unknown outcome is
  shown and nothing is sent again.
- Dashboard: the IAM dialogs state the permanent revocation record a disable,
  group change, password reset or delete keeps, and the cluster-wide limit at
  which deletes, disables and new sign-ins are refused.
- Client-group record views (split horizon): an endpoint with `kudurru: { view:
  <ClientGroup> }` is now served only to clients of that ClientGroup (in the
  record set's namespace), replacing the default answer of the same name and
  type for them. Other clients keep the default answers, and view-only names do
  not exist for them. Works on the SQLite and Kubernetes drivers.
- Dashboard: **Filtering > Inventory** lists every device the client inventory
  knows, including devices that never sent a query: devices reported by
  inventory integrations and the answering node's DHCP leases, with MACs, names,
  tags, observation and expiry, and the state of each address in words
  (assigned, expired, ambiguous, superseded, withdrawn). Filters for source,
  kind, staleness and a text search; each device links to the Integration or
  DHCP scope that reported it and to the client page of every address the query
  log knows. On a SQLite cluster member the page says that DHCP leases and lease
  MACs are the answering node's own, for every session that may read the
  inventory. Needs `inventory.read`; the Clients page links to it.
- Dashboard record editor: "Served to" picks a record's view: every client
  (default data), published only (`public`), or a ClientGroup of the record
  set's namespace. For a group view the editor says that views are not served in
  a DNSSEC-signed zone, that transfers, exports, RFC 2136 UPDATE and integration
  sinks carry only the default data, and that the dnsdist edge bypasses its
  packet caches while any group view exists; types a view cannot hold (NS,
  DNAME, SOA, DNSSEC types, an apex CNAME) are refused before saving. Record
  lists show each record's view and link the group. Where the record list is the
  served zone (SQLite store driver), the page and the editor say that a public
  record, or a view the server refuses, is stored but not listed. A group of
  another namespace than the record set's is refused before saving.
- `GET /api/v1/integrations` summaries carry `inventorySource`, the source ID
  the incarnation's devices carry in `GET /api/v1/inventory`.
- `DELETE /api/v1/zones/{zone}/records/{name}/{type}` takes `?view=` (a
  ClientGroup name, `public`, or `-` for the default data) to delete one view
  only, and `?namespace=` to delete from the record sets of one namespace only.
- `GET /api/v1/inventory` answers `nodeLocalLeases`: true when the answering
  node is one member of a SQLite cluster, so its DHCP leases and lease MACs are
  its own.
- Metric `kudurru_root_anchor_updater_acquisitions_total{outcome}` counts the
  root-anchor updater's queries to the root servers as `applied`, `raced` (an
  ordinary write landed while the reply was in flight, so the node asked
  again), `failed`, `refused` (the updater stopped for its term on the reply
  or on a store error) or `canceled` (the writer term or the process ended
  during the query).

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
  texts (allow and deny lists, identity administration, authenticators, unblock
  decisions, import and migration runs and
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
  that the local query log could not be flushed; when it is not flushed in
  time, the sinks are reported in one warning, `additional query-log sinks
  stopped because the query log did not flush`. The collector warns when a
  sink loses queries, at the first loss and at each tenfold total.
- A live Integration with `deletionPolicy: Delete` stored without the reserved
  finalizer is shown as `Paused` with `CleanupFinalizerMissing` and makes no
  provider call until the next load of its unchanged document adds it.
  Declaring a `Terminating` Integration with `deletionPolicy: Retain` (or
  without the line) releases it without contacting the provider, logged as
  `objects: finalized` and audited as `integration.finalize`. Check, preview,
  sync and actions are refused for a `Terminating` Integration. The objects
  loader requests the deletion of a file-managed object once. On Kubernetes
  the same lifecycle runs on the replica that holds the Integration's Lease
  (D367, D384).
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

- On SQLite, a long `kudurru apply` or `kudurru restore` (and a multi-delete in
  the dashboard) pauses for other callers' reads: when another client's read
  is waiting, the request lets the node publish before its next document, so
  the read is answered instead of failing with `503 generation_pending`. Any
  waiting `GET` or `HEAD`, signed in or not, triggers a pause; the pauses are
  bounded and make a request at most about twice as slow. Publishing repeats
  less work: readers of one stored generation reuse its digest, canonical
  form and object identities, within bounded caches (D378).
- `kudurru login --username` works against an enrolled follower and no longer
  reads `GET /auth/status` first (D368).
- A blocking switch sent through a follower whose view of the cluster
  settings is older than the primary's answers the primary's `409 conflict`
  and changes nothing. A follower whose connection to its primary is refused,
  or whose primary's host is reported unreachable, answers a forwarded change
  with `503 unavailable` and "was not applied", because nothing was sent;
  `503 completion_unknown` remains the answer once a connection was made, or
  while a connection attempt is still waiting at the deadline. A member
  fenced by data that diverges from its primary's at the same revision still
  answers `503 cluster_conflict`, and its message names the divergence
  (D368).
- Tier-B webhooks: the event body and the client and network inventory JSON
  use lowercase field names (`{"topic","time","payload"}`; the old capitalized
  names still decode); each webhook instance has its own provider request
  budget; a webhook whose peer offers only `records` is refused unless
  `spec.webhook.domains` is set inside the peer's domain filter; a reviewed
  `integrations sync` of a webhook with domains needs `--mode publish` and the
  preview digest.
- An Integration that kudurru refuses outright, with `deletionPolicy: Delete`
  and no cleanup finalizer, reports its refusal reason instead of
  `CleanupFinalizerMissing`.
- Adding, changing or removing a zone's `kudurru.io/zonegroup` label needs
  `zones.write` at cluster scope; a zone admin of the zone or the group can no
  longer move it between groups. On Kubernetes, if the label is not the
  requested one after such a write, the API answers 409 (D385).
- On Kubernetes, authenticator apps (TOTP) and the local-user create and
  password-reset commands are not offered (use your identity provider's
  second factor); the routes keep answering `503
  factor_management_unavailable` and `user_management_unavailable`, and
  `/auth/status` says so up front. The Helm chart refuses
  `auth.kubernetes.mode` set through `config.node` or `config.settings`, and
  `impersonate` with `mode: standalone`; kudurru refuses `impersonate` with
  `store.driver: sqlite` (D387).
- `kudurru edge run` uses the internal listener credentials whenever
  `--internal-port` is set, also with `--poll-zones=false`. Revoking an edge
  block clears dnsdist's dynamic block table and reinstalls the remaining
  operator blocks, so clients the `--dynblocks` rate rules had blocked are let
  through until they exceed the rate again (D388).
- Downloading the query log (`GET /api/v1/queries/export`) needs the
  `querylog.export` permission (`auditor` and `admin`); the `operator` role
  still reads the query log but no longer exports it, as the role table
  always said. Allowing or denying a domain from a logged query
  (`POST /api/v1/queries/{id}/actions`) also needs `querylog.read` (D393).
- After a node's first 0.2 start on SQLite, a start no longer logs
  "settings applied" for cluster keys of `kudurru.yaml` that are not in
  effect: for differing keys that apply without a restart it warns
  `cluster settings in kudurru.yaml are not in effect` with the stored values
  now in effect (list and object values left out), and for differing keys
  that need a restart it warns `cluster settings in kudurru.yaml differ from
  the cluster settings object` without claiming a value (D373).
- `kudurru anchors status --offline` also prints the owner's keys, the
  recovery record generations, the member's local denials and its authority
  mode. The recovery runbook says how recovery works in an enrolled cluster
  (recover on the stopped primary only; followers take the recovered state on
  their next pull), and "A member returns with different trust-anchor state"
  in `docs/deploy/sqlite-join.md` covers every state in which a member stays
  fenced or answers `SERVFAIL` and what ends it (D369).
- Dashboard: `503 primary_required` is shown as "Needs the primary" with the
  primary's name and `503 completion_unknown` as "Outcome unknown", also on
  the Settings page; after a change sent from a follower's dashboard the
  pages are read once more about six seconds later; the node's role is reread
  every 30 seconds and at once after a `primary_required` answer.
- Documentation: the migration guides, the cutover checklist and the admin
  guide describe this release (one `kudurru apply` applies every document; on
  SQLite the API refuses unserved zone types and unusable update policies; a
  list whose URL cannot be downloaded serves no rules and reports the error);
  `docs/deploy/dynamic-updates.md` lists every update-policy shape SQLite
  refuses at write; the enrolled-cluster page matches the follower contract;
  the stored TSIG key and dynamic-update guides use `kudurru tsigkeys`; the
  Enterprise page keeps the Cloudflare and UniFi integrations on the roadmap.
- Contributor tooling: the live dnsdist edge suites use the upstream dnsdist
  2.1.3 image by registry digest; the benchmark README lists every hosted
  benchmark run and how the final paired run on the mixed runner pool counts.

- DHCP: a reservation can move a client that leases another address
  instead of being refused until the lease expires; the client moves to it
  when it next renews or reconnects. **DHCP** > **Leases** has a **Reserve**
  action that prefills the reservation from a lease.

- Dashboard: a fenced member (`503 cluster_fenced`) and a member in a promotion
  conflict (`503 cluster_conflict`) are named in error banners and on the
  sign-in page, with a link to the procedure that ends the state.
- Dashboard: zone and zone-group grants from `/auth/me` now enable the matching
  record, NOTIFY, delete and settings actions on that zone's pages. Before, only
  cluster-wide permissions counted. **Zones** and a zone's page show the zone
  group.
- Dashboard: where `/auth/status` reports that the driver has no authenticator
  management (Kubernetes), the sign-in page has no authenticator code field, the
  **Authenticator** section says that no authenticators are offered, and
  **Identity** has no authenticator recovery.
- The `integrations.write`, `integrations.delete` and `integrations.secrets`
  permissions are now in use (previously reserved). A write that binds or
  changes an Integration's `spec.credentials` needs `integrations.secrets` in
  addition to `integrations.write`, because a reference can name any credential
  the node allowlists; so does changing the type, the UniFi endpoint or TLS pin,
  or the webhook endpoint of an Integration that has credentials, because that
  decides which server receives them. Reading an Integration
  (`integrations.read`) shows the credential reference names it uses, never
  values. Only the built-in `admin` role holds these permissions.
- Creating an Integration needs a license that covers integrations (`402
  license_required`); replacing, suspending and deleting an existing one do not.
- `kudurru backup` and `kudurru export` without kinds still cover only the DNS
  objects and need no integration permission; `kudurru export integrations`
  exports Integrations.
- `docs/deploy/dnssec-validation.md`: the statement that no RFC 5011 worker runs
  now applies only to forwarded validation, and the page links to managed root
  anchors for recursive groups.
- The admin guide and the access architecture page now say that every change
  of a zone's `kudurru.io/zonegroup` label through kudurru on Kubernetes
  (adding, changing or removing it) takes the whole zone object over and
  removes the fields that kubectl or a GitOps tool set and the change does
  not contain.
- CI runs the version-synchronization unit tests in the go job's test step with
  the other `hack/` tests instead of in a separate helm-job step.
- The README marks v0.2.0 as in development, lists the Cloudflare and UniFi
  integrations as part of it instead of planned, and links the enrolled SQLite
  cluster guide from the deployment table.
- The capability matrix (`docs/deploy/capabilities.md`) and the architecture
  overview say that Cloudflare publication and UniFi inventory run on the
  Kubernetes store, on the replica that holds the Integration's Lease, and the
  `unavailable` error reference no longer lists integrations on Kubernetes.
- The deployment index links the Enterprise custom branding page, and the
  maintenance chapter of the administrator guide points to the ordered upgrade
  notes for 0.1.x.
- Disabling, deleting or changing the groups of a local account, and disabling
  or deleting a service account, now end its sessions and delete its API tokens
  in the same commit; enabling restores none. A service account token is shown
  once and only its hash is kept; it expires no later than the API token that
  issued it, and none is issued for the bootstrap account or for accounts
  another manager owns.
- On Kubernetes the new commands answer `503 iam_management_unavailable` or `503
  user_management_unavailable` with a hint; change those objects with `kubectl`.
  On an enrolled follower they answer `503 primary_required`.
- Each deleted account or provider, and the first disable or group change of an
  account, keeps a permanent revocation record. These records share a limit of
  about 4,000 entries with active sessions; at the limit, deletes and new
  sign-ins answer `503 unavailable` and change nothing.
- The integration manifests list no refused UniFi fields on SQLite; on
  Kubernetes an Integration that enables a UniFi write is refused with reason
  `UniFiPushUnsupportedOnKubernetes`.
- A UniFi Integration now polls at the shorter of `spec.unifi.poll` and
  `spec.resync` (default ten minutes); `spec.resync` was ignored before, so a
  poll longer than ten minutes now runs every ten minutes unless `resync` is
  raised.
- A review or command whose captured input still names a record set or zone
  deleted since answers `409` (review changed) instead of `404 integration not
  found`.
- An integration intent whose acknowledged provider effects have all been
  removed, with the destination back at its baseline, is now settled at the next
  poll for publishers that cannot resume an intent (the UniFi record mirror),
  instead of staying pending.
- An Integration that enables `push.dhcpDNSServers` must now name `dnsServers`
  and `networks`, and `push.mirrorRecords.enabled` requires `domains`; both were
  refused outright before.
- An external zone serves only the records derived from its Kubernetes sources:
  the API refuses a DNSRecordSet for an external zone with `conflict`, and a
  record set written another way (kubectl) is reported `NotSelected` and not
  served. RFC 2136 UPDATE into an external zone is refused with `NOTAUTH`. The
  SQLite store still refuses the `external` type. Each source kind is read as a
  complete list of at most 4,096 objects and 32 MiB (every Service counts); a
  kind over the bound keeps its record sets but stops following its sources.
- Dashboard: controls that release an Integration are disabled in static mode,
  as the API refuses them there.
- UniFi: when two clients of one poll claim the same address with the same
  observation time, the inventory now lists that address as `ambiguous` on both
  devices instead of dropping it silently, and an older claim of another
  inventory source can no longer take over the address. Neither device
  identifies queries from it, and no DNS record is published for it, as before.
- Kubernetes RBAC: the auditor and editor ClusterRoles (chart and kustomize)
  gain read access (`get`, `list`, `watch`) to the virtual resource
  `inventories`, which backs `inventory.read`.
- The enrolled offline recovery acceptance (`anchors recover --offline` on the
  stopped primary, the follower adopting the recovered image through scheduled
  pulls) now runs in the default end-to-end suite in minutes instead of only as
  an hour-long opt-in soak. Test binaries built with `-tags licensetest` accept
  `KUDURRU_TEST_ANCHOR_UPDATER_AHEAD`; release binaries do not.
- Dashboard: the identity actions are disabled with the reason on a follower, in
  static mode, without the permission, and on the Kubernetes driver (GET
  /auth/status capabilities).
- Zone transfers, zone exports, RFC 2136 UPDATE and integration sinks
  (Cloudflare, UniFi) carry the default data only; client-group view records
  never leave the server through them. Cloudflare no longer selects endpoints
  that belong to a client-group view.
- Any client-group view makes the dnsdist edge bypass both its authoritative and
  its resolver packet cache (`zone-client-view`), because the edge cache cannot
  tell clients' groups apart.
- The `DNSRecordSet` schema now requires `kudurru.view` to be `public` or a
  valid ClientGroup name.
- Views are not served in DNSSEC-signed zones, cannot hold NS, DNAME, SOA or
  DNSSEC records, and cannot sit at or below a delegation or DNAME (including,
  on SQLite, DNSSEC data or a delegation added by RFC 2136 UPDATE). A zone
  serves at most 16 views; the budget is shared by every record set of the zone,
  first come first served. On Kubernetes such endpoints are reported on their
  record set; on SQLite only when nothing else in the record set is served.
- A client-group view may replace only default data of its own namespace: a view
  endpoint that would override another namespace's name and type is refused as
  `Duplicated`, as the same default endpoint would be. Views of one name in two
  namespaces refer to two different ClientGroups and do not conflict.
- A view name (and the empty non-terminals it creates) suppresses a default
  wildcard for that group's clients, as any existing name does.
- Integrations: a check, preview, sync or action reviewed just before the
  integration host wrote its own status now runs instead of answering `409` once
  the host has read that write back (within one capture interval, two seconds by
  default; until then a reread and one retry may still be needed), and `GET
  /integrations/{namespace}/{name}` keeps showing the status of a healthy
  running instance after such a write (`statusKnown` stays true). A change of
  the spec, labels, annotations or finalizers still makes the review stale.
- Integrations: `GET /integrations/manifests` gains `fieldLimits`, one entry per
  spec field this node's host refuses (with the status reason and message) or
  accepts without effect (with the field it needs). On Kubernetes the refused
  UniFi record mirror field is now `spec.unifi.push.mirrorRecords.enabled`, and
  `spec.unifi.push.dnsServers`, `networks` and the record mirror's `types`,
  `domains`, `labels` and `exclude` are listed as inert.
- Integrations: the UniFi `dnsServers` schema now gives the canonical dotted
  IPv4 pattern the provider accepts and names the addresses it refuses.
- Integrations: the audit row of a UniFi `block-client` or `unblock-client`
  action names the client in lowercase (`for client 02:ab:cd:00:00:01`),
  whatever letter case was sent.
- Refusals that waiting does not change no longer carry `Retry-After: 1`: the
  Kubernetes answers `cli_login_unavailable`, `provider_logout_unavailable`,
  `factor_management_unavailable`, `user_management_unavailable`,
  `sessions_unavailable`, `external_users_unavailable` and the last-fallback
  `unavailable` of the authentication routes, and the `unavailable` answers of
  cluster enrollment, invitations and promotion on a node that cannot have them
  (static, standalone, Kubernetes). Status codes and keys are unchanged.
- The operator page `docs/deploy/capabilities.md` explains both meanings of
  `501 not_implemented`: an integration provider without the command, and a
  server started without the part of the runtime that the message names.
- `docs/deploy/capabilities.md` lists the API service account, service
  account token and identity provider commands, with the Kubernetes refusal
  `503 iam_management_unavailable` and what to do instead (change the
  `ServiceAccount` and `IdentityProvider` objects with `kubectl`). The local
  user row now covers changing and deleting a user.

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
  no answer, so the client waited for its own timeout. Empty SERVFAILs that
  the node produces itself are not released this way: a node whose default
  upstream group is `mode: recursive` keeps resolving cache-miss lookups while
  a configuration change is committed, where lookups in the tens of
  milliseconds after a commit used to be dropped (D377).
- `Delete` cleanup: a long record-change history is forgotten in commits sized
  to the 30-second run; a run that a slow store keeps from finishing is
  reported as `CleanupFailed`, while a run whose provider request does not
  answer within the bound is `ProviderFailed`, also when a partial answer
  could not be recorded in time; a failed first count keeps
  the counts last reported; a provider delete cut before its receipt is
  confirmed by a reread and recorded in UTC (D367).
- Query-log HTTP sinks read the answer of a destination that refuses a large
  batch before reading it and resets the connection, and believe a 2xx only
  for a body that was written whole: a 2xx that arrives before the body was
  sent is an unknown outcome, counted as dropped and not sent again. With a
  `timeout` under about 4 seconds a large batch is sent at once instead of
  waiting for `100 Continue`, and a refusal at the headers is still heard.
- The release workflow installs the envtest binaries before its test job; the
  primary-zone runtime tests fail without them.
- The dashboard no longer says that MAC address, host name and name pattern
  matchers "never match" or "arrive in v0.2", and the settings help and the
  RoleBinding CRD reference no longer promise `kudurru login` or zone groups
  for v0.2 (zone groups now grant access; see Added). The Authenticator panel says "Authenticator management is
  unavailable on this server" on Kubernetes.
- Documentation: eleven sequence diagrams of the architecture tour did not
  render; `docs/deploy/kustomize.md` named the wrong tree for v0.1.0; the
  migration guides said that `kudurru query` shows an allow-list decision; the
  administrator guide called the Identity tab read-only; and several pages
  still called enrolled replication, Kubernetes TSIG rotation or durable
  Kubernetes revocation open.

- On SQLite, DNS keeps answering for the node's own zones while a long apply,
  restore or multi-delete commits its documents: answers come from the
  configuration the node published last, and a document is answered once the
  node has published it. Before, a request of 120 documents could leave
  existing records unanswered for over a minute. The same holds after every
  API write: right after a write is acknowledged, DNS can still answer the
  previous data (or `NXDOMAIN` for a new name) until the node publishes, and a
  node that cannot publish keeps answering it and reports `prepare_failed` in
  `kudurru health --ready`. DNS answers also no longer stop for a few seconds
  when the node replaces its sign-in provider resources or its license state
  changes (D378).
- A primary zone on the SQLite runtime keeps answering its committed records
  after every change to its data. In the half second or so after a record
  change or a dynamic UPDATE (while the zone's own status was written), the
  zone, and a parent zone that delegates to it, used to answer `SERVFAIL`
  (`servfail_unsynced`) (D377).
- A record set at a name that a dynamic update owns no longer freezes its zone
  on SQLite: only the endpoint at that name is left out, the record set's
  other endpoints are served, and each left-out endpoint is named as
  `Duplicated` in the record set's own conditions (`Degraded` while other
  endpoints are served, not `Ready` when none is). The dynamic record keeps
  answering; delete it or the endpoint to resolve it. The check stays
  proportional to the zone's size with many dynamic DNAMEs (D373).
- A block list whose published body a SQLite node lacks no longer stops the
  node from serving: the list is served empty until its body is stored, and
  the node logs this once for the source. A list URL that accepts the
  connection and never answers no longer delays the status of other lists or
  the first download of a list added while it hangs (D373).
- A query signed with a wrong, replaced or expired TSIG key gets its NOTAUTH
  (BADKEY, BADSIG or BADTIME) reply also when it arrives between a commit and
  the node's next publication; before, it got no reply. Refusals of UPDATE,
  NOTIFY and zone transfers are unchanged.
- A query-log filter (client, domain, upstream, list, zone, policy or a
  suggestion prefix) that contains a NUL byte is refused with 400 instead of
  matching every row (`*%00` was evaluated as `*`) or failing on the central
  PostgreSQL log.
- An import that also writes objects or settings no longer stops partway with
  "current writable serving generation unavailable" when a background change
  (such as the zone lifecycle update after an imported zone) is committed just
  as the next step starts: the step waits for that change to be published and
  continues; a step that has started writing still stops on a refusal and is
  never repeated. An import step that runs longer than 5 seconds on a busy node
  is no longer canceled ("owner bounds exceeded"); the 5-second limit applies
  only to waiting for the node's current configuration.
- SQLite: when write authority changes (activation, a fence by a higher term,
  a promotion conflict, an uncertain enrollment completion or closing the
  store), the leadership term ends before the new authority is visible, so the
  node no longer logs a spurious `leader-only runnable failed; restarting` and
  counts no restart; a leader-only task that stops with an error after its
  term ended no longer leaves it as the task's last error.
- Managed root anchors: the elected updater no longer stops for the rest of
  the writer term when an ordinary change, such as a zone status update at
  startup, is committed while it reads or acquires the root key set; it
  discards that reply and acquires again, waiting 1, 2, 4 and up to 64
  seconds while the race repeats, and logs each retry ("root anchor update
  raced an ordinary commit"). A follower (or fenced or conflicted member) no
  longer stops its root revocation checks for the rest of its process when an
  ordinary change from the primary is installed while its check starts (D369).
- On a follower, fenced or conflicted node, the block page shows a client's
  pending or declined unblock request with its status again; the request form
  stays hidden there (D368).
- Dashboard: a sign-out that the server could not confirm (for example on a
  follower that cannot reach its primary) no longer fails silently; the login
  page says that the session may stay valid until it expires. A sign-out that
  cannot reach the server at all keeps you signed in and says so.
- Documentation: the client-identity page, the gateway runbook and the AdGuard
  Home guide said that every CIDR group on the arriving address hides a client
  ID; a catch-all `0.0.0.0/0` or `::/0` group does not. The upgrade runbook's
  rollback steps now always remove a persisted `local.db` after
  `helm rollback` and take IAM and AuditLog state from the pre-upgrade backup.
- Tests and CI (contributors): the helm job's `make deploy-assets` check no
  longer needs the `v0.1.0` tag (the 0.1.0 chart fixture is regenerated only
  with `-update-v01-chart` and checked against the tag separately); mounted
  serving fixtures stop their always-on runtime after the writer and no longer
  report `browser writer ended context canceled` or `providerresources: owner
  closed`; fixture clocks are swapped without racing the runtime; test
  listener ports come from `internal/testutil/testport` (outside the
  ephemeral range, held by a cross-process lock), so `bind: address already
  in use` is gone from the mounted and e2e tests; the TOTP previous-step codes
  come from the server clock; the dnsdist supervisor test's fake health window
  fits one console round trip; and the live dnsdist tests wait for the backend
  to finish committing before their single-shot queries.

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

- A zone keeps answering while a change to its own `DNSZone` (query ACL,
  name servers, selector) is being applied. A primary zone answers its last
  committed records under the new query access rules (zone transfers wait for
  the new configuration), and a forward zone uses its new configuration.
  Before, the zone answered SERVFAIL for up to about 0.3 seconds after each
  such write.
- A DNS query that arrives just before the node publishes a configuration
  change now gets its answer from the data it arrived on. Before,
  about one query in a few thousand around each publication got no reply
  and the client timed out.
- With managed root trust anchors, a freshly installed node no longer stops
  its root anchor updater ("root anchor updater stopped for this writer
  term: replication: authority changed") when its first-boot setup commits
  while the updater starts. The updater now tries again a second later, so
  the node keeps refreshing its anchors without a restart.
- An invalid Integration document never repeats a value of its
  `spec.credentials` in the error (from `PUT`, `POST /apply` or a follower): a
  validation failure of a credential reference is answered with the field path
  only, so a credential value pasted into a `file` or `env` field by mistake,
  including one with a trailing newline, a quote or a backslash, is not echoed,
  relayed or logged.
- Dashboard: a session without `cluster.read` no longer requests `GET /cluster`
  every 30 seconds (each refused read was written to the audit log as a denial
  on every node); it follows the node's role through the public `GET
  /auth/status`, and a `403` ends the poll for any session and is not retried on
  navigation or window focus. The Nodes page no longer reads `GET /cluster`
  without `cluster.read`; its Cluster panel says so instead.
- Dashboard: the invitation of the "Invite a member" wizard can be closed with
  the close button, Escape or the backdrop (after the "Close the invitation?"
  prompt) also when the primary's answer took a moment; before, only "Done"
  closed it.
- Dashboard: an `?add=` link to the Allowlist or Denylist no longer opens the
  add dialog briefly on a follower before the node's role is known, and the link
  is dropped from the address when the session cannot follow it.
- Dashboard: on a follower, only a forwarded change (not a sign-in, a DNS query
  from Tools or a change the follower runs itself) schedules the second read of
  the page six seconds later.
- Dashboard: the audit log renders a page of 100 events several times faster:
  its cells no longer each carry a right-click menu (rows still expand with a
  click, the chevron, Enter or Space).
- A zone-group member or a zone admin whose `PUT` of a zone was in flight
  while an administrator deleted that zone no longer creates the zone again
  (with its group label) on Kubernetes. Creating a zone always needs
  `zones.write` at cluster scope, and the write now decides this on the read
  it is compare-and-swapped against.
- Record set and zone lists, and `GET /auth/me`, answer `503` when the group
  of a zone they need cannot be read, instead of `200` with that zone's items
  or verbs left out.
- With `auth.kubernetes.mode: impersonate`, only the apiserver's own
  "cannot impersonate" refusal of kudurru's identity is reported as `503`
  (kudurru lacks the verb). A refusal for a principal whose name contains
  that phrase, or an admission webhook message that uses it, is the
  principal's `403` again.
- The Helm guide's kustomize impersonation example closes its code block, so
  the rest of the page renders again.
- Dashboard: an RPZ source is no longer shown as an inline source ("ConfigMap"
  or "0 inline entries", "inline, nothing to fetch"); the sources table names
  its zone and primaries and offers no entries browser for it. A list whose only
  fetched sources are RPZ feeds shows its last refresh as "per node" and no
  refresh interval, since feeds follow their zone's SOA timers.
- A recursive upstream group set in the configuration file
  (`default: { mode: recursive, timeout: 4s }`) no longer stops the first start
  of a node on SQLite with "strategy: Unsupported value". A recursive group now
  has no strategy in its settings.
- A managed-anchor primary that was just restarted, promoted or chosen by
  `cluster resolve`, and whose root-anchor refresh is not due, no longer
  refuses writer commands with 503 "current writable serving generation
  unavailable" while its anchor updater makes its first step: it starts its
  writer services at once. A writer whose refresh is due (for example after a
  long outage) still finishes that refresh first, so a root key revocation it
  observes is committed instead of being lost to a startup write; such a node
  can refuse writer commands for up to 30 seconds after it reports ready
  (D369).
- A root anchor capture that raced an ordinary commit now logs its own
  consecutive count ("root anchor capture raced an ordinary commit; capturing
  again") instead of the update-race count, and only at the first, second,
  fourth, eighth and so on consecutive race, so a capture that keeps racing no
  longer logs a warning every second.
- Cluster members with managed root anchors no longer answer `SERVFAIL` to
  validated recursion and zone queries while they persist a newly observed
  fence, promotion conflict or divergence. DNS keeps answering from the served
  publication and accepted trust until the DNS-only publication takes over;
  writers and the HTTP API still close at once, and a fence that cannot be
  written to disk still stops DNS as before.
- Dashboard: after a sync with an unknown outcome, an Integration's page now
  holds further syncs until a Check reports the endpoint reachable and the
  credential accepted; a Check that reported either one failing used to lift the
  hold.
- Endpoints whose `kudurru.view` named a ClientGroup were silently withheld from
  every client.
- Through a follower, an item of `kudurru apply`, `kudurru restore` or a
  dashboard multi-delete that the follower cut because its data changed now
  says what to do: it names the enrolled primary and asks to send the items
  that were not applied there, because running the file again through the
  follower stops the same way (D375).
- A forwarded write whose answer the primary sent, but which the follower
  could not relay because its own data or membership changed during the
  exchange, is now answered `503 completion_unknown` ("the write may have been
  applied") instead of `503 unavailable` "cluster authority changed", which
  claimed that nothing was made (D375).
- A token caller's password change through a follower is always answered for
  the caller's account, as on the primary, whether or not the calling token
  still works afterwards, and it waits for the follower's next publication
  instead of answering `503 committed_output_unavailable` while one is being
  prepared; a browser session the follower could not issue after a committed
  change now names the cause in its `committed_output_unavailable` answer
  (D368).
- A signed RFC 2136 UPDATE sent through an enrolled SQLite follower during a
  burst is no longer answered SERVFAIL when the follower installs the
  primary's previous commit just before it passes the update on: the follower
  waits for its current publication and sends the update once. An UPDATE
  through a follower still answers SERVFAIL when it could not be sent or
  answered within the four-second forward bound (a slow publication on a
  loaded primary counts here), when the follower cannot reach its primary or
  is not admitted by it, when the primary committed the update but could not
  publish it, or when the zone is retiring. The follower never sends an
  update twice; retrying an idempotent update is safe.
- A repeated delivery of the same forwarded UPDATE is answered with the
  primary's retained answer even when the primary's publication changes at
  that moment, instead of SERVFAIL.
- A follower whose pull was canceled or timed out in the middle of an
  install no longer refuses every later pull with "store/sqlite: anchor
  lifecycle unavailable" until it is restarted.
- A follower's pull right after a commit on the primary is no longer refused
  and logged as "replica: unauthorized peer"; the primary reads the image
  again.
- A connection attempt to the primary that times out is reported as an
  unknown outcome in every case, as documented, instead of sometimes as "not
  applied".
- Shutting down a node with managed root trust anchors no longer logs "root
  anchor updater stopped for this writer term: sql: transaction has already
  been committed or rolled back".
- A primary zone and a primary child zone under it on one node (for example
  `example.` and `child.example.`) no longer leave one or both answering
  SERVFAIL until the process restarts. They converge in either order of
  creation and across deletion and re-creation of the child, and the parent
  keeps answering its own names while its child is being added.
- A new zone, and in particular a new child zone, starts answering sooner on a
  node with many zones: the zone driver now takes a zone that is being created
  through its steps first instead of queuing each step behind every other
  zone. Until its first publication, names under a new child zone answer
  SERVFAIL (which resolvers retry within seconds), not the parent's negative
  answer (which resolvers would cache for the parent's negative TTL).
- Integrations: an instance whose provider setup failed (for example a provider
  that was briefly unreachable, or a setup cut short while a node was starting)
  is set up again with backoff (from one poll interval up to five minutes)
  instead of staying `ProvisionFailed` until its object changed. A setup whose
  peer offers no role the instance can take is not retried.
- Integrations on Kubernetes: a `deletionPolicy: Delete` UniFi Integration that
  gained `spec.unifi.push.dhcpDNSServers` or `clientActions` is now cleaned up
  when deleted instead of staying Terminating until released; a refused push
  gets its own condition message.
- Integrations: a refused Integration document no longer repeats a pasted secret
  used as a map key under `spec.credentials`; known credential names are still
  named.
- External zones: on the SQLite store the `SourcesNotRead` message no longer
  points at `externalZones.enabled`, which that store refuses, and a zone
  already Degraded for a more specific reason (for example `Duplicated`) keeps
  that reason.
- `POST /api/v1/zones/{zone}/records` with a `kudurru.view` replaced the default
  endpoint of the same name and type in the record set (and the reverse); the
  view is now part of the endpoint's identity, so a view is added beside the
  default data.
- On the SQLite store driver, `GET /api/v1/zones/{zone}/records` (the served
  zone) did not list client-group view records; they are now listed with
  `kudurru.view` set to the ClientGroup's name and `namespace` set to its
  namespace.
- The `mac` matcher help in the dashboard said that only an inventory
  integration supplies MACs; it now names the built-in DHCP server's leases and
  their node-local scope on a SQLite cluster.
- Every `503` that a supported deployment answers now has a hint that says what
  to do: the Kubernetes session, authenticator, external-account and local-user
  refusals, cluster enrollment, invitations and promotion on a static or
  standalone node, an unknown or unready OIDC provider for browser and
  command-line login, DHCP reads on a node without DHCP, the VIP event stream on
  a node without a VRRP address, and integration administration on Kubernetes.
- Every `501 not_implemented` now has a hint, and so do the `503 unavailable`
  answers for a server without the block page renderer or the DHCP setup
  manager, for query suggestions, status and actions without query history, and
  for cluster-wide query actions without a central database.
- Rollup recovery evidence (`GET /querylog/rollup-evidence` and its `verify`)
  on a node without a central database now answers `503 unavailable` with a
  hint that names `database.dsn` and `database.querylog.ship`, instead of a
  bare "central rollup recovery unavailable".
- A DHCP setup write (`PUT /node/dhcp/v4/setup`) on a Kubernetes release with
  DHCP off now says to set `dhcp.enabled` and `dhcp.serverAddress` in the Helm
  values, as the setup read does. On a SQLite node that is not on Linux it
  says that DHCP runs only on Linux, instead of asking for a setting that
  does not help.
- A node asked for a peer's health now explains a failure: without
  internal credentials or a peer address it names the internal listener
  settings, and when the peer is down, times out, refuses the credential or
  runs a version without the route (`peer health: unsupported`) it says what
  to check, or to read the health on that node itself. History and live-tail
  reads that no requested node answered name the internal credentials, the
  query log on each node, and `scope=node`.
- `GET /integrations/manifests` on a server without the integration runtime
  answers with the same hinted refusal as the other integration routes.
- `GET /inventory` on a server started without the client inventory now
  says where it is mounted and names `GET /api/v1/clients`, instead of a bare
  "the client inventory is not mounted on this server".
- An enrolled SQLite primary answers a forwarded signed dynamic update
  `SERVFAIL` instead of `REFUSED` when its keyring changes under the update;
  nothing is applied and the client sends it again.

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

- Upgrading from 0.1.x has its own ordered checklist, "Upgrading from 0.1.x"
  in `docs/deploy/runbooks.md`: back up first (0.1.0 refuses every database
  0.2 has opened); SQLite stops an upgrade on stored objects 0.2 refuses;
  apply `kudurru-crds` first (five new CRDs); the offline Kubernetes IAM
  migration; the AuditLog key ring; `store.kube.expectedClusterID`; projected
  ServiceAccount tokens of at most 600 seconds; central database schema 6;
  the new Compose files; BlockLists stored by 0.1 keep `refreshInterval: 24h`;
  the optional separate SQLite storage.
- During a host-by-host upgrade of static twins, a 0.2 twin lists a 0.1.0 twin
  on **Nodes** with its version and readiness and reports its health detail as
  `peer health: unsupported`; both keep answering from their own files. A
  0.1.0 node that connects to a central database a 0.2 node has migrated
  reports `CentralIncompatible`, keeps answering DNS and keeps its query log;
  once upgraded it sends the backlog once.
- On SQLite (Docker, systemd, Compose, the chart's `standalone` mode) the first
  0.2 start copies the cluster settings of `kudurru.yaml` into the node's
  database, and from then on they live there: change them with
  `kudurru settings set` or the Settings page; host keys and objects still
  follow the files. Static twins (`cluster.role: static`, Compose HA) refuse
  settings writes, so after a twin's first 0.2 start none of its cluster
  settings can be changed: put every cluster setting a twin needs, including
  the managed anchor settings, into `kudurru.yaml` before that host's first
  0.2 start.
- Managed root anchors stay off after the upgrade. Enable them with
  `upstreams.recursion.managedTopology` in the node file and the anchor
  settings through `kudurru settings set` or the Settings page (on static
  twins, in `kudurru.yaml` before the first 0.2 start).
- A RoleBinding with scope kind `zonegroup`, or an `rbac.bindings` entry with
  that scope, must now name a valid label value (at most 63 letters, digits,
  `-`, `_` or `.`, starting and ending with a letter or digit). Such a binding
  granted nothing before 0.2.0; before you upgrade, rename it to the label
  value you will put on your zones or delete it. kudurru refuses to start with
  an invalid `rbac.bindings` entry, and a stored RoleBinding with an invalid
  name is refused on its next write (D385).

### Known limitations

- Right after a node reports ready, and while it prepares a publication, a
  sign-in, a cluster activation, a read or another command can be answered
  `503 generation_pending` or `503 unavailable` ("current writable serving
  generation unavailable") without effect; repeat the request.
- A node may answer a DNS query from a configuration that was replaced up to
  one second earlier (the data was committed and current when the query
  arrived), and while a zone's own `DNSZone` change is applied a primary zone
  answers its last committed records under the new query access rules
  (D395).
- Until a new primary child zone under a primary parent on the same node is
  first published, names under it answer `SERVFAIL` (a few seconds on a
  loaded node with many zones; resolvers retry). The parent keeps answering
  its own names (D404).
- A signed RFC 2136 UPDATE through an enrolled follower can still be
  answered `SERVFAIL` when it cannot be sent or answered within the
  four-second forward bound (a slow publication on a loaded or newly
  promoted primary counts here), when the follower cannot reach its primary,
  when the primary committed the update but could not publish it, or when
  the zone is retiring; one test run also saw an early `SERVFAIL` whose cause
  is not known. The follower never sends an update twice; retrying an
  idempotent update is safe.
- IAM writers on SQLite: every deleted account or provider, and the first
  disable or group change of an account, keeps a permanent revocation
  record. About 4,000 such records fill the security section shared with
  active sessions; deletes and new sign-ins are then refused with
  `503 unavailable` and no API command frees the space.
- Integrations on SQLite: every integration capture reads the whole stored
  configuration, so on a large store a capture that takes longer than
  10 seconds stops every Integration job, and a `Delete` cleanup whose
  commands take longer than 30 seconds does not finish; the store size at
  which this starts is not measured. UniFi optional writes are refused on
  Kubernetes, and a UniFi record-mirror create that failed partway is
  recovered automatically only when the policies it created are gone.
- Dashboard: **Edit** on a served client-group view record of a namespace
  other than `default` (SQLite) saves the record into the `default`
  namespace's group of the same name; use `kudurru apply` with the record
  set of that namespace instead.
- On SQLite, while a long multi-document request commits, DNS answers from
  the configuration published last, but on a heavily loaded node other API
  callers' reads can still be refused with `503 generation_pending`, and one
  test run in about a hundred has shown a gap of 1.4 seconds without a local
  answer, cause unknown.
- Through an enrolled follower, a file that does not fit between two of the
  follower's pulls (five seconds by default) or inside the 30-second request
  is cut: later documents report `unavailable` and are not applied, earlier
  ones stay committed, and repeating the command through the follower does
  not finish it. Send such a file, and every restore, to the primary (D375).
- A write through a follower whose exchange with the primary fails after a
  connection was made, or whose connection attempt is still waiting at the
  request's deadline, answers `503 completion_unknown`: inspect the object
  before a new decision. A refused connection answers `503 unavailable`, and
  nothing was applied.
- On an enrolled follower, OIDC, browser and device sign-in, authenticator
  setup, the allow and deny list commands, unblock decisions and stored TSIG
  key commands run on the primary only (`503 primary_required`); change a
  list through a follower by writing its BlockList object.
- An enrolled cluster has no command that removes a member: a replaced member
  stays listed, keeps its name and internal address reserved and counts
  toward 64 active members, so a replacement needs a new name and address.
- Managed root anchors: when the primary can never record a root-key
  revocation that one member saw, that member never follows the primary
  again; it keeps answering DNS from its own last-good data and must be
  replaced. No command records a revocation on the primary by operator
  decision.
- Integrations on Kubernetes: UniFi record publication and Tier-B webhooks are
  refused; commands run only on the replica that holds the Integration's
  Lease; a cleanup interrupted by a Lease handover and manual finalizer
  removal are documented but not qualified on two replicas. On SQLite, above 100,000 stored objects or
  256 MiB of stored documents no Integration job runs, a `Delete` cleanup
  included.
- An unpinned Kubernetes node started while the API server is unreachable
  exits before it answers; set `store.kube.expectedClusterID` so that it can
  serve from `local.db`.
- Managed root anchors behind managed edges: adding an empty follower behind
  dnsdist edges is tested against a real dnsdist only in an opt-in live test,
  not in CI.
- The pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3 request whose QUIC
  end of stream (STREAM FIN) arrives in its own empty frame after the query
  data; the client waits until it times out and the query never reaches
  kudurru. Clients that send the FIN with the query are answered, and
  kudurru's own DoQ and DoH3 listeners answer both forms.
- Managed root anchors: a member whose own root check saw a key revocation
  that the root no longer publishes answers `SERVFAIL` for good, also as the
  primary, and while it is the primary no member's trust anchors are updated.
  Promote a member that serves and replace it.
- Active Directory DNS replacement remains unfinished preview work. The owner
  deferred the remaining implementation and real Windows qualification to a
  future effort; no production AD replacement or Windows support matrix is
  claimed.

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
