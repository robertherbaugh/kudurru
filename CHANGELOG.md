# Changelog

All notable changes to kudurru are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Until v1.0 the API group is
`kudurru.io/v1alpha1` and may change between minor releases.

## [0.1.0] - unreleased (release candidate 0.1.0-rc.1, not tagged yet)

### Licence (before the first release)

- kudurru is now **source-available** under the PolyForm Noncommercial License
  1.0.0 instead of Apache-2.0. Personal use is free; a business needs an
  agreement ([LICENSING.md](LICENSING.md), D155). Nothing had been published,
  so every tag, release, image and chart carries the new licence; commits
  before the change keep the old one in the history.
- **License keys** (D156): a free personal key (3 nodes) or a business key
  unlocks more than one node and the features that moved to [`ee/`](ee/):
  OIDC sign-in, the dnsdist and keepalived edge, and the integrations. Keys are
  verified offline; with no key kudurru runs one node, and DNS answering never
  depends on a key. New: `kudurru license status|verify`, `--license-key`,
  `KUDURRU_LICENSE_KEY(_FILE)`, `GET /api/v1/license`, `402 license_required`
  for configuration writes over the node limit, `kudurru_license_*` metrics,
  Helm `license.*` values and the Compose `KUDURRU_LICENSE_KEY` variable.
- Also behind a key: roles beyond the one built-in administrator (role
  bindings, scoped roles, Kubernetes RBAC), the audit trail (storage, API and
  export; the audit log lines stay), statistics older than 24 hours (the
  rollups are still collected), and unblock requests (the block page stays
  free). Without a key at most 2 policies can be created through the API.
- Code moved: `internal/edge` to `ee/edge`, `internal/auth/oidc` to
  `ee/oidc`, `internal/integration` to `ee/integration`, the binding source,
  SubjectAccessReview authorizer and ClusterRole generator to `ee/rbac`, and
  the sqlite audit recorder to `ee/auditstore`.
- Contributions need a DCO sign-off and a relicensing grant (CONTRIBUTING.md).

The first release, "stone": a daily-driver DNS server that runs as one
container or as a Kubernetes Deployment with CRDs. It covers milestones M1 to
M5 of [docs/architecture.md §13.1](docs/architecture.md), with the gaps that
section lists. The milestone reports in [docs/m1](docs/m1/README.md),
[docs/m2](docs/m2/README.md), [docs/m3](docs/m3/README.md) (M3 and M4, D122)
and [docs/m5](docs/m5/README.md) hold the measurements, and decisions D79 to
D126 record what changed on the way.

### Dashboard and API fixes (v0.1 fix round)

Fixes made before the first tag, after the dashboard was first run against
a real server instead of its mocks.

- Dashboard against the real API: Settings shows the real values (switches
  showed OFF for enabled keys) and warns when a change is not persisted or
  needs a restart; the dig tool renders answer records (it showed
  `undefined`), latency, flags and errors; the Clients, Nodes and Audit
  pages no longer crash; API tokens can be created again; zone import, the
  Diagnosis links, live-tail resume and sign-out work. The wire types are
  generated from `api/openapi.json` (`bun run gen:api`, D128).
- New dashboard pages and actions: **Upstreams** (groups, servers, strategy,
  timeout, per-server health and RTT, bootstrap resolvers, conditional
  forwarding, ECS), change password (and the forced change after a password
  reset), query-log purge, creating a zone from a zone file; Policy and
  forward-zone upstream groups are picked from the configured groups.
- Dashboard look: the kudurru brand (logo, favicon, app icons and a default
  "Kudurru" theme with the Clay accent); readable hover and selection states
  in every theme; every dialog scrolls, keeps its buttons visible and has a
  close button.
- API: `/iam/*` lists users, service accounts, role bindings and identity
  providers; `GET /auth/me` reports the real roles and `mustChangePassword`,
  and the API's own routes refuse a session that must change its password
  (403 `password_change_required`); `GET /audit?order=desc`; `last_seen` on
  client rows; `GET /auth/tokens/{id}`; "Domains on lists" counts the
  compiled block lists; the OpenAPI document of the forwarded `/auth` and
  `/iam` routes matches the handlers (D127); zone deep links with dots load
  the dashboard instead of a 404.
- Zones can be deleted: `DELETE /zones/{zone}` (and `kudurru zones delete`)
  removes the zone with every record set that references it, in one
  transaction on the sqlite driver; `?dryRun=true` reports the record set
  and record counts first. Zones or record sets owned by files,
  integrations or Kubernetes refuse with `managed_by` and the root zone `.`
  with `default_zone`. `DELETE /objects/dnszones/{name}` no longer orphans
  record sets: a zone they reference answers 409 `zone_not_empty` (D148).
- The zone page has Delete zone (with a dry-run preview and the zone name
  typed to confirm) and a Zone settings section that edits what the wizard
  set (name servers, TTLs, SOA, query access; a forward zone's upstream
  group and DNSSEC validation); the Zones list deletes per row. Record
  deletes confirm in a dialog.
- A password change (`revokeTokens`, a checkbox in the dialog) or `kudurru
  admin reset-password --revoke-tokens` can also revoke the user's API
  tokens.
- Blocked services and safe search (D60, D120, D124-D126):
  `Policy.spec.blockedServices` from an embedded, kudurru-authored catalogue
  of 32 services, and `safeSearch`/`safeSearchMode` for Google, Bing,
  DuckDuckGo, YouTube, Yandex, Brave and Pixabay; `blocking.catalogue.url`
  replaces the catalogue at runtime; `GET /catalogue`.
- Store, zones and transfers: the 1.5 MiB object cap in every store driver
  (D130); zones re-reconcile at their next record expiry; per-message write
  deadline on TCP/DoT streams including zone transfers (D129);
  `BlockList.status.unsupported` and `spec.sources[].subdomains`; local.db is
  stamped with the store's last contact.
- Edge and VIP: keepalived and dnsdist configurations refuse unusable
  addresses; a keepalived restart race that crashed the new child is fixed.
- Testing and docs: a Playwright suite drives every dashboard page against a
  real `kudurru serve` (`make web-e2e`, D123); a Hugo documentation site
  with generated settings, CRD, CLI and API references (`make docs-site`,
  D121).
- Release engineering: the `kudurru-keepalived` image builds again
  (keepalived 2.4.3 configured with `--disable-iptables --disable-nftables`),
  `make edge-check` passes and a failing `edge` workflow now fails the check;
  distroless bases pinned by digest; the release workflow verifies the ko
  image signatures; serial-divergence, edge and VIP panels and the
  `KudurruSerialDivergence`, `KudurruEdgeBackendDown` and
  `KudurruVIPNoMaster` alerts; a kind + MetalLB smoke test workflow.

### Dashboard pages, wizards and the block page (v0.1 UX round)

- Every object a user configures has its own page and every multi-concept
  create flow is a wizard in a modal over the page you are on (`/…/new`
  links open it there); other dialogs are kept for short actions (D132,
  D142). Policies (`/policies/:ns/:name`: active now, schedule,
  shadowing, groups, lists, a blocked-services logo grid, safe search, 24 h
  activity), lists (`/lists/:ns/:name`), groups, clients (which group and
  policy apply and why), upstream groups and zones; wizards for new
  policies, lists, groups, zones and upstream groups (with provider presets
  for Cloudflare, Quad9, Google, AdGuard, Mullvad, NextDNS, Control D,
  OpenDNS).
- Lists: a Pi-hole-style enable switch per source
  (`BlockList.spec.sources[].enabled`; a disabled source is not fetched or
  applied and keeps its cache), an inline entries browser backed by
  `GET/PATCH /blocklists/{ns}/{name}/sources/{source}/entries`, and a
  Domains page.
- Typed controls instead of free text: every settings key carries labels,
  sections, allowed values, bounds, presets, formats and nested shapes
  (`GET /settings/_schema`, `kudurru config schema`, the docs reference), and
  the same metadata validates the file, the environment and the API (D133).
  The Settings page and the forms render dropdowns, duration presets,
  bounded numbers and object pickers from it.
- Policy state: `GET /policies/{ns}/{name}/state`; the query log records
  the deciding policy per row (querylog.db schema 2, upgraded in place) and
  filters on `policy=`.
- Service, search-engine and provider logos from homarr-labs/dashboard-icons
  (Apache-2.0), embedded, sanitised and served at
  `/api/v1/catalogue/icons` (D134).
- Block page (D135): the `blockPage` block response sends blocked web
  requests to a kudurru page on the node (`listeners.blockPage.*`,
  `blockPage.*`) that explains the block (list, rule, service, policy) within
  the query-log privacy mode; HTTPS is optional and always shows a
  certificate warning. Admin preview in Settings.
- Unblock requests (D136): a Request access form on the block page; a
  Requests page (`/requests`) to approve (everyone or a group, exact or with
  subdomains, permanently or for a time) or deny; the `UnblockRequest` kind
  and the `unblockrequests` RBAC resource; `unblockRequests.*` settings.
  **Kubernetes: install the new CRD (`kudurru-crds` chart) before upgrading
  the server.**
- Blocked services: the catalogue grows to 150 services in 14 categories
  (AI with 19 providers, social, messaging, video, music, gaming, shopping,
  dating, gambling, news, forums, productivity and cloud storage, VPN and
  proxies, crypto exchanges), with logos. A policy can block whole
  categories, including services added later (`blockedCategories`), with
  exceptions (`allowedServices`). The grid has category tabs with counts,
  search by name or domain and a "Blocked only" filter (D145). Content
  categories from the Block List Project (gambling, adult, piracy, phishing,
  malware, …) are offered as downloaded lists (D146). Opt-in: import
  AdGuard's HostlistsRegistry services at runtime
  (`blocking.catalogue.adguardServicesURL`, GPL-3.0, downloaded by your
  instance, never shipped) (D147). The Allowlist shows entries switched off
  in a single list as list exceptions, and row switches sit on the right
  (D144).
- Allowlist and Denylist pages (Filtering), next to the combined Domains
  page: one row per entry with an enable switch, type (exact, wildcard,
  regex), scope (everyone or client groups), comment, origin and the list it
  lives in; bulk actions and an "Is it allowed?" check. Single entries of any
  list, imported URL lists included, can be switched off
  (`spec.sources[].exclude`, kept across refreshes); the entries browser
  opens for every source type. Existing `ui-allow`/`ui-deny` entries are
  migrated (D143).
- Every form control is an Astryx component (Selector, NumberInput, Switch,
  CheckboxInput/CheckboxList, RadioList, MultiSelector, Typeahead,
  DateTimeInput, TextInput/TextArea) through `components/fields/`, with the
  helper text under the control so rows align on their inputs; raw form
  elements are a lint error. Settings rows split 60 % explanation / 40 %
  value, and every key, option and nested field carries plain-language help
  (a test fails on a missing one) (D141).
- Themes built the Astryx way instead of CSS overrides on `:root`: the
  default Kudurru theme is a `defineTheme` theme over Astryx Neutral whose
  colours are stops of a palette generated from docs/brand
  (`astryx theme palette generate`), Neutral, Stone and Y2K extend the Astryx
  packages, and all four are built themes wired through the generated
  `src/astryx-themes.ts`; the query status colours are theme tokens. Same
  look; the themes name only system fonts (the packages' Google Fonts never
  loaded under the CSP) and the Astryx reset stylesheet is loaded, which
  evens out some spacing.

### Security review (v0.1 round)

[docs/m5/security-v0.1-round.md](docs/m5/security-v0.1-round.md) reviews the
block page, unblock requests, allowlists, catalogue fetchers and the release
pipeline.

- Release pipeline: `publish` checks out `refs/tags/<tag>` without persisted
  credentials, runs only from the release branches with a valid tag, builds
  the supervisor images from the verified kudurru digest, and every
  `cosign verify` pins `publish.yml@refs/heads/release(-candidate)`.
- The guarded list client checks every request URL, redirects included, so a
  redirect through an HTTP(S) proxy cannot reach cloud metadata; the
  catalogue refreshers are covered too.
- The AdGuard import keeps only single-site `||name^` rules and at most 1000
  services.
- Unblock requests: object names are keyed (a viewer could recover the
  requester's address from an allow-list entry); a re-submission racing an
  approval no longer makes a time-limited approval permanent; the end of an
  approval leaves a manual entry with the same rule alone; the block page
  matches the client's own pseudonym.
- Allowlist and entries APIs: no partial delete when a copy is managed
  elsewhere; linear source patches and list exceptions; at most 4
  concurrent parses of cached list bodies per node.
- The approve dialog and API say that an everyone approval is attached to
  every policy kudurru may edit.
- Authentication caches: a deleted local user or token could be put back
  into a node's cache by the background `lastLoginAt`/`lastUsedAt` update
  that read it just before the deletion, and stayed valid there until the
  next re-watch; a deletion now refuses older copies for a minute.
- Query log: the in-memory domain counts of the current hour are capped
  at 100,000 names; a flood of unique names (random subdomains over TCP,
  which the rate limiter does not cover) grew them by every name, about
  2 GB an hour at the query log's peak rate. A full hour is read from the
  fact table instead.
- Zone records: `POST /zones/{zone}/records` and the record delete
  sometimes answered 409 to a single client writing records one after
  another (the record set's status changed between the API's read and its
  write); the API now re-reads and re-applies, and answers 409 "try again"
  only when every attempt loses.
- DNS-over-TLS upstreams: Cloudflare's 1.1.1.1 answers a burst of about
  30 queries on one TLS connection and then REFUSED, and the connection was
  reused, so with only `tls://1.1.1.1` configured about a third of the
  names in a burst failed with SERVFAIL. A REFUSED answer from a DoT server
  is now asked again once on a fresh connection.
- Release pipeline: every action is pinned by commit SHA (Dependabot keeps
  them current) and goreleaser, helm and cosign by version; the tests run
  in a separate read-only job without a Go cache before anything is
  published; every image gets build provenance and the Dockerfile-built
  images an SBOM; a release refuses a candidate whose publish did not
  finish (`hack/candidate-published.sh`).
- Zone import and export: an SOA RNAME with an escaped `\@` in a label was
  split there as if it were an e-mail address, so a re-import changed it
  (found by `FuzzImportBIND`).
- Container images: a fresh Docker named volume at `/var/lib/kudurru`
  (`docker run -v kudurru-data:/var/lib/kudurru`, every compose file) came
  up root-owned and kudurru exited with `open state.db: permission
  denied`. Every image now has `/var/lib/kudurru` and `/run/kudurru` owned
  by uid 65532; the ko images build on `kudurru-base` (images/base, D154)
  and `make volume-check` checks each image.

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
  alert rules (`deploy/`), including the serial-divergence panel and alert.
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
  validated over 42 value permutations with kubeconform; kustomize bases
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
- Release branches (D139): pull requests against `main` push a
  `dev-<PR>` image; merging into `release-candidate` tags
  `v<VERSION>-rc.<N>` and publishes a pre-release; merging into `release`
  tags `v<VERSION>`, publishes the release and moves `latest`. `VERSION` in
  the repository root is the single version source. Release binaries and
  images now embed the built dashboard (goreleaser runs `make web-build`),
  and the `<major>.<minor>` image tag is gone.
- Binary-size budget in CI (`make size`).
- Host networking: `ghcr.io/robertherbaugh/kudurru:<version>-hostnetwork`
  (and `latest-hostnetwork`) is the release binary with the file capability
  `cap_net_bind_service=+ep`, built from the ko image by `publish.yml`; the
  chart's `hostNetwork.enabled` runs it with `NET_BIND_SERVICE` added and
  `allowPrivilegeEscalation: false`, so uid 65532 binds :53 without the host
  sysctl (`make hostnetwork-check`).
- The supervisor images copy the binary from `/ko-app/kudurru`, where ko
  puts it (they copied a path only the local Dockerfile image had); the
  Dockerfile now uses ko's layout.
- Edge: `kudurru edge run --dnsdist-user`; with `edge.ebpf.enabled` and on
  the host network (edge DaemonSet, keepalived, compose HA) the dnsdist
  container starts as root with a minimal capability set and dnsdist
  switches to uid 953 after binding and creating the BPF maps (eBPF could
  not create its maps, and the host-network edge could not bind :53, as
  uid 953). With `serviceMonitor` or `podMonitor` the chart scrapes the
  supervisors (:8084, :8085) through a `<release>-edge` PodMonitor.
- The dashboard is embedded precompressed (gzip, optional brotli): the
  binary shrinks by 1.8 MB.

### Documentation

- User and administrator guide in [docs/guide](docs/guide/README.md): the
  block page and access requests for users, and install, daily work, the
  command line, maintenance and troubleshooting for administrators. It is
  written in Simplified Technical English (ASD-STE100) with screenshots of
  the dashboard and of the block page. `python3 hack/guide-lint.py` checks
  sentence and paragraph length, unapproved words, links and figure numbers;
  `web/scripts/guide-screenshots.mjs` takes the screenshots again. The docs
  site lists the guide under **Guide**.
- The docs site (docs.kudurru.io) is deployed by Cloudflare Workers Builds
  (`site/wrangler.jsonc`, `site/static/_headers`; setup in
  [site/README.md](site/README.md#publishing)) instead of GitHub Pages.
  `docs-pages.yml` and its call in `release.yml` are removed, and the docs
  workflow runs `make docs-check-workers`. The guide's screenshots are files
  of the site, not links to GitHub's raw host, so they load while the
  repository is private.
- The project website, kudurru.io, in [www/](www/README.md): one page with
  the lineage, an interactive walk through the query pipeline, the v0.1
  features with their measured numbers, the dashboard screenshots from the
  guide, the deployment shapes and the roadmap, linking to docs.kudurru.io
  and the repository. Bun and TypeScript, rendered at build time, no inline
  script or style; `make www-check` tests it. Cloudflare Workers Builds
  deploys it as static assets from the `release` branch.

### Known limitations

See [docs/followups.md](docs/followups.md) for the full list. Notably: no
DNSSEC signing or validation (v1.0), no follower/raft replication (v0.2),
cluster-scope stats are per node, the blocking toggle is per node (v0.2), the
dnsdist console diff-apply is v0.2, and the two-node VRRP
behaviour still needs runs on real hardware
([docs/release-checklist.md](docs/release-checklist.md)).
