# M5 work package: edge, CLI, chart and docs follow-ups, v0.1.0 release prep

Key `burndown-edge-release`. Branch based on 347f4c7. Every commit keeps the
module green (build, vet, golangci-lint on the touched packages, race tests).

## What changed

| Area | Change | Commit |
|---|---|---|
| CLI | `newHealthCmd` installs the VRRP hook: `kudurru health --vrrp` runs the probe, and keepalived.conf renders the §3.6 spelling by default (`keepalived.DefaultProbeCommand`); `kudurru vip probe` stays as the same probe | e8b36b4 |
| edge tests | Root cause of a deterministic `TestSupervisorPeersFile` failure in this sandbox: the fake keepalived announced `start pid=` before `signal.Notify`, so the test's immediate peers-file change delivered SIGHUP to a process without a handler and killed it. The fake now installs its handlers first (real keepalived does too) | e8b36b4 |
| CLI | `kudurru token generate [--out file]` prints a fresh `kdr_` token (`--out` writes a new 0600 file, never overwrites) | f5bab87 |
| CLI | `kudurru lameduck [duration]` (default 5s; Go duration or seconds) posts `{"seconds": N}` to `POST /lameduck` on the ops listener (`--ops-address`, default 127.0.0.1:8081), then waits N seconds unless `--no-wait`; 404/405 gives "this server predates the lameduck endpoint". Written against the contract in the brief; the server side belongs to the burndown-server package | f5bab87 |
| CLI, edge | `kudurru doctor` gains a `vrrp` check: WARN when `vip run --legacy` is in use (the supervisor now keeps `<run dir>/vrrp-legacy` while it renders without `auth_hmac`, readable from the kudurru container through the shared volume) or when a readable keepalived.conf (`--keepalived-config`) has VRRP instances without `auth_hmac`; OK when every instance is authenticated; silent on nodes without keepalived | f5bab87 |
| rbac, chart | `make rbac-manifests` (`ee/rbac/rbacgen`) writes `charts/kudurru/templates/rbac-aggregated.yaml` from `permissions.yaml`; the hand-written copy in `rbac.yaml` is gone. `TestAggregatedRolesDrift` fails on a stale file and `TestKubernetesRules` checks every (role, resource, verb, named) against the verb SubjectAccessReview asks | cda4844 |
| chart | The release-namespace Role gets `patch` and `delete` on Secrets | cda4844 |
| compose, chart | HA compose files name their static backends (`--backends=node-a=10.0.0.11:5353,...`), the edge sidecar names its backend after the pod (`$(KUDURRU_NODE__NAME)=127.0.0.1:<port>`), so the dnsdist validation function checks the answering node id. The `name=addr` form itself already existed in `cluster.ParseStaticEndpoints`; the edge ServiceAccount already had endpointslices get/list/watch | cda4844 |
| chart, deploy | `auth.oidc.providers` renders `auth.identityProviders` (validated: `id` required; `clientSecretRef` only with mode=kubernetes); a new `oidc` chart permutation (34 now); `externalDNS.webhook.listen` left `pendingKeys` (the key is in the schema), which is now empty | 6bd90e9 |
| edge | `dnsdist.CacheFlusher` (`NewLocalCacheFlusher(apiURL, keyFile, log)`): `FlushResolver(ctx)` sends `DELETE /api/v1/cache?pool=resolver&name=.&suffix=true` with the key from `/run/kudurru/dnsdist-api.key`; `BlockingChanged(until)` flushes in the background now and again when a temporary disable ends; `ErrNoLocalEdge` without a local edge; `Stop()` | b546c0d |
| api | The Scalar reference is self-hosted: the MIT bundle of `@scalar/api-reference` 1.44.20 (the version huma pins; SRI identical to huma's, checked by a test) is embedded gzipped (983,832 bytes) and served at `/api/v1/docs/scalar-1.44.20.js` (immutable cache, gunzipped for clients without gzip). The page honours `listeners.api.pathPrefix`; its CSP allows script, style, font, image and connect sources from `'self'` only (plus `'unsafe-eval'` and inline styles, which Scalar needs, as huma's own CSP allowed); web fonts, telemetry and the AI agent are off. NOTICE and `internal/api/scalar/README.md` record licence and provenance | da0b4db |
| build | Binary-size budget re-baselined: `make size` already failed at 347f4c7 (53,862,560 bytes against 51,234,000 from M1); new budget 60,362,000 = rc.1 size 54,874,272 + 10 % | 6955b36 |
| build, chart | Charts at `0.1.0-rc.1` (version and appVersion); `RELEASE_VERSION := 0.1.0-rc.1` in the Makefile is what a build without a reachable tag reports (`kudurru 0.1.0-rc.1 (<commit>, ...)`; previously the commit hash or `dev`); kustomize bases regenerated | c9dc65f |
| docs | architecture.md: §7.1 steps 3, 4, 5, 9, 11, 13 restored (decision text had overwritten them; the decisions stay in §16), §3.3 Request/Response snippet matches the code (`UpstreamGroup`, `Policy`, `Client.TSIGKey`, `Transfer`), §6.2 keys (internal TLS files, `listeners.sockets`, `upstreams.ecs`, `blocking.listsDir`, `ratelimit.*`, `auth.kubernetes.mode`, `auth.adminSocket`), §11.1 metrics (`store_watch_catchups_total`, `store_watchers`, `ratelimit_buckets`, dropped reasons), §8 internal kinds path, Node principals, `POST /auth/password`, `/auth/status`, §5.1/§5.3 generated ClusterRoles and Secret verbs, §5.2 pointer to D105; D104 now names `health --vrrp` as the rendered check | 5723ccc, e5bba03 |
| CLI | The chart rendered supervisor flags the binary did not accept, so every edge pod would crash-loop: `kudurru edge run` now takes `--readiness-address` (alias of `--health-address`) and `--dot/--doh/--doq/--doh3 host:port` (enable those frontends with explicit ports); `kudurru vip run` takes `--node-name` (alias of `--router-id`) and `--peers-from nodes:<selector>` (`keepalived.NodePeers`: InternalIP, else ExternalIP, of the VIP family; priority index from the node's position by name or its `kudurru.io/vip-priority-index` label; `<run dir>/vrrp-peers` re-listed every 30 s and watched by the supervisor). `TestChartSupervisorArgs` parses the chart's argument lists | 1be1842 |
| docs | followups.md regrouped (see below); CHANGELOG.md with the v0.1.0 section by area; docs/release-checklist.md | e5bba03, 770db06 |

## Follow-up rows closed

Deleted with the commits above: the whole "M1 exit criteria carried over"
table (done since M2); duplicated sqlite, object-cap, condition-reason and
`cluster.role` rows; `installVRRPHealth` wiring; named static backends and
endpointslice RBAC; `kudurru doctor` legacy warning; Role delete on Secrets;
`kudurru admin …` + `token generate`; the architecture rows (§6.2/§8 auth,
§7.1/§3.3/§6.2 ECS, §11.1 metrics, deploy decisions); Scalar from unpkg;
`externalDNS.webhook.listen`; aggregated ClusterRoles; the CLI rows for
`health --vrrp`, `vip drain`, `admin reset-password`, `backup`/`restore`.
Narrowed: edge cache flush (hook done, serve wiring and remote edges open),
`lameduck` (client done, server endpoint and chart `preStop` switch open),
OIDC/`cluster.role` (OIDC done), §11.6 metrics (`edge_backend_up` and
`vip_master` exist), supervisor flags (accepted; images unpublished). The
hardware-bound items now live in one section at the end.

## New findings (rows added)

- `POST /api/v1/auth/password` is served by the auth stack and named in the
  mustChangePassword hint (`internal/auth/authz.go`), but `internal/api`
  `registerAuth` does not forward it, so through the API listener it answers
  404 and a first-boot admin with a generated password cannot change it over
  the API. Two lines plus `make openapi`; outside this package (api owner).
- `images/keepalived/README.md` still names `kudurru vip probe` (outside this
  package; both commands work).
- goreleaser builds only the kudurru image; the supervisor images have no
  release job. Whether `docker_signs` with `artifacts: manifests` covers the
  ko images needs checking on the first real release.

## Shared files

- `docs/architecture.md`: in-place edits in §3.3, §5.1, §5.2 (one added
  paragraph), §5.3, §6.2, §7.1, §8.1, §8.2, §10 (one route), §11.1 and the
  D104 text. No decision rows were added, so no D-number can collide.
- `Makefile`: `rbac-manifests` target, `RELEASE_VERSION`, `SIZE_BUDGET`.
- `NOTICE`: Scalar entry; the huma entry no longer claims the docs page.
- `docs/deploy/helm.md` (chart documentation): the RBAC paragraph and the
  `auth.oidc` sentence.
- `cmd/kudurru/main.go`: two command registrations. `serve.go` untouched.

## Integrator notes

- Flush hook: create `dnsdist.NewLocalCacheFlusher("", "", logger)` in
  serve, call `BlockingChanged(until)` wherever the blocking `Toggle`
  changes (the API toggle and the replicated `Apply`), `FlushResolver`
  after a list refresh, and `Stop()` on shutdown. It only reaches an edge
  on the same host or pod.
- Lameduck: once `POST /lameduck` exists on the ops listener, switch the
  chart's `preStop` from `sleep` to `exec: [kudurru, lameduck,
  <lameduckSeconds>s]` and restrict the endpoint to loopback peers (the ops
  listener has no authentication).
- `--peers-from`: the priority index is fixed at start so a re-render never
  moves a running node's priority. A node that joins later takes its sorted
  position, which can equal an existing node's index; pin
  `kudurru.io/vip-priority-index` labels for fixed fleets.

## Measurements

| Item | Value |
|---|---|
| Stripped linux/amd64 binary at 347f4c7 | 53,862,560 bytes |
| Stripped linux/amd64 binary at the end of this package (placeholder dashboard) | 54,874,272 bytes (90 % of the new budget) |
| Scalar bundle | 3,544,608 bytes raw, 983,832 bytes gzipped (+1,011,712 bytes of binary) |
| Docs page check | headless Chrome against `kudurru serve` with `pathPrefix: /kudurru` over TLS: reference rendered, 0 CSP violations (the AI agent's `proxy.scalar.com` calls were blocked until it was switched off) |
| `goreleaser check` (2.18.3) | 1 configuration file validated |
| `goreleaser build --snapshot --single-target --clean` | succeeded; `kudurru 0.0.0-SNAPSHOT-c9dc65f` (no tags in the worktree) |
| `kudurru version` from a build without git metadata | `kudurru 0.1.0-rc.1 (unknown, go1.27.2, linux/amd64)` |
| Chart permutations | 34; `make helm-check`: 630 chart resources and 115 kustomize resources valid against Kubernetes 1.30 |
| Full `CGO_ENABLED=1 go test -race ./...` (envtest on) | green except `internal/controller` `TestMeasureRecordSetChanges/resync…` once under load average 13-15 (unowned package, passes alone; followups row added) |
| `golangci-lint run ./...`, `go vet ./...` | 0 issues |
