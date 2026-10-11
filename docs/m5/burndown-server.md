# M5 work package: server-side follow-up burn-down

Package key `burndown-server`, branch `worktree-wf_b932d6b4-504-3`, based on `347f4c7`. It closes ten server-side rows of docs/followups.md, in the order of the assignment.

Commits:

- `6bac88f` config: settings layer, Reloadable, watcher
- `348ce2a` cache, blocking and ratelimit reload; lameduck SERVFAIL in ingress
- `b6a46a8` querylog privacy and retention at runtime
- `faf3b11` serve: hot reload, settings writer, `/lameduck`
- `ac0cc81` `kudurru lameduck`
- `c081b5f` `kudurru.io/enabled`
- `8c7cacf` `/internal/zones` and the zone metrics
- `24611c3` list refresh and query-log purge
- `48891e8` leader address from the EndpointSlice
- `05cf343` `GET /api/v1/doctor`
- `d540f91` drops send nothing
- `9d5ab28` controller test flakes
- `b57859e` audit prune loop
- `e5fbd9f` followups
- `3ae4bd0` memory driver resourceVersion check and conformance case

This report (`eeaeb84`, then this update) closes the series.

## 1. Settings hot reload (D25)

### The settings layer (`internal/config/reload.go`)

- New node key `settingsFile`: hostOnly, restartRequired, default `""`. `KUDURRU_SETTINGS_FILE` overrides it; it is a reserved variable, not a `__` key.
- `Load` reads its layers in this order:
  1. defaults
  2. the node file
  3. the settings file
  4. the environment

  So the environment still locks a key.
- `Config.SettingsFile` holds the path that took effect.
- The settings file may only contain cluster-scope keys. Node-scope and unknown keys are an error: at start the load fails, and on reload the file is ignored. A missing or empty file is an empty layer.
- Other helpers in the package:
  - `ReadSettingsFile`
  - `WriteSettingsFile`: writes the changed keys atomically (temp file, fsync, rename, mode 0640). It keeps the other keys but drops comments.
  - `Flatten` and `FromFlat`: the flat dotted-path form that the API already used.
  - `MergeCluster(cur, fresh)`: takes the cluster-scope keys from `fresh` and keeps the node-scope keys of `cur`.
  - `Changed` and `ChangedUnder`.

### Reloadable and the watcher

- `config.Reloadable` holds the effective `*Config` in an atomic pointer.
  - `Subscribe(fn(old, next, changed))`: subscribers run synchronously, in registration order, one `Set` at a time.
  - `Set(next)`: notifies only when something changed.
  - `Reload(environ)`: re-runs `Load`, then `MergeCluster`.
- `config.WatchSettings(ctx, r, WatchOptions)` watches the settings file with fsnotify on its directory, which covers the Kubernetes ConfigMap `..data` symlink swap and editors that rename the file.
  - It also polls every 10 s.
  - It debounces events by 200 ms.
  - It compares a sha256 of the content before reloading.
  - An invalid file is logged, and the last good settings stay in effect.

### What applies without a restart (`cmd/kudurru/reload.go`)

`subscribeReload` applies these groups:

| Keys | How |
|---|---|
| `upstreams.*` (groups, bootstrap, conditional, ecs) | builds a new `upstream.Set` with the same `Metrics`, `upstreamstage.Stage.Swap`, flushes the response cache, then `prev.CloseReplacedBy(next)` after a 30 s grace |
| `cache.*` | new `cache.Cache.Reconfigure`: atomic stale-TTL and prefetch limits, per-shard capacity resize that evicts at once. The cache stage gets `SetSettings` (TTL clamps, stale timers) and `SetDisabled` (`maxEntries: 0` passes queries through and flushes). serve now always builds the cache and its stage. |
| `blocking.enabled`, `defaultResponse`, `blockTTL`, `customIP.*` | `blockingstage.Stage.Reconfigure` (atomic defaults) |
| `ratelimit.*` | `ingress.RateLimiter.Reconfigure(*RateLimitConfig)`: nil disables, and every bucket is dropped. serve always builds the limiter, so `ratelimit.enabled` can change at runtime. |
| `querylog.privacy`, `anonymize`, `exclude.*` | `querylog.Async.SetPrivacy` plus `sqlitelog.Writer.SetPrivacy` (what later batches store) |
| `querylog.retention.raw`, `maxDBBytes` | `sqlitelog.Writer.SetRetention`. The reader tier, the status and the size-cap loop read the live values. |
| `log.level` (node scope, via PATCH /node/config) | the serve logger's `slog.LevelVar` |

`restartRequired` in the metadata table is now truthful. These keys newly carry it:

- `log.format`
- `querylog.enabled` and `querylog.sink` (they decide which sink exists)
- `blocking.allowDegraded` and `blocking.refreshInterval` (read by the list resolver and fetcher at construction)
- `auth.tokens.maxLifetime` (read when the auth stack is built)

`config.TestRestartRequired` pins the set of hot keys. Adding a key to it means adding a subscriber. The `settings-get` CLI golden file changed with it: `log.format` now shows `R`. A PATCH or a file change that touches a restart-bound key is logged as "take effect after a restart".

### Writes through the API

- `api.Options.Reload`: when set, the API reads `Reload.Current()` and applies a PATCH with `Reload.Set`. A PATCH and a file reload therefore go through the same subscribers.
- `api.Options.Settings` (the settings writer) is now called for `PATCH /settings` only. `PATCH /node/config` changes stay in memory (`persisted: false`). hostOnly keys are refused as before.
- serve's `settingsWriter`:
  - **kube driver:** 409 `managed_by` from `api.ErrSettingsManaged("Kubernetes (the kudurru-settings ConfigMap)", ...)`.
  - **settings file configured:** `config.WriteSettingsFile` writes the changed paths. The watcher then sees no difference.
  - **neither:** nil, so changes stay in memory (`persisted: false`).
- `api.Options.Upstreams` became the interface `api.UpstreamStatus`. `*upstream.Set` still satisfies it, and serve passes `api.UpstreamStatusFunc` over the swapped set.

**Smoke run.** `kudurru serve` with a settings file:

- Rewriting the file logged `settings applied keys=[cache.maxEntries ratelimit.slip]`.
- `PATCH /api/v1/settings {"blocking":{"blockTTL":"30s"}}` returned `persisted: true` and rewrote the file with all three keys.
- The process exited 0 on SIGTERM.

## 2. D51 drain path

- `ingress.Stage.Lameduck func() bool`: while it returns true, any IN-class query for `health.kudurru.internal.` answers SERVFAIL. dnsdist's must-resolve check (2 failures at 1 s) then drains the node from every pool, while every other name keeps resolving.
- serve wires it to `health.Checker.IsLameduck`. The runner's shutdown lameduck (`checker.Lameduck()`) therefore sets the ingress flag too.
- `health.Checker` changes:
  - New `SetLameduck(bool)` and `IsLameduck()`.
  - New routes on the ops listener: `POST /lameduck`, `DELETE /lameduck` and `GET /lameduck` (`health.PathLameduck`). The POST and DELETE routes are **loopback peers only**, because the ops listener has no authentication.
- `kudurru lameduck [duration]` (`cmd/kudurru/lameduck.go`): POSTs to `--ops-address` (default `127.0.0.1:8081`), then waits for the duration. `--cancel` leaves lameduck.
- **Verified.** `dig` returned SERVFAIL for the health name during lameduck while `home.example SOA` still answered, and `/healthz/ready` returned 503.
- **preStop story.** The chart's `preStop: sleep` only drains the Service endpoints. dnsdist keeps sending queries until the process stops answering. docs/deploy/runbooks.md and compose-ha.md now say so. Switching the chart to `exec: ["kudurru", "lameduck", "<n>s"]` is a followups row for the deploy owner, because charts/ is not in this package.

## 3. `kudurru.io/enabled: "false"`

**Decision: an annotation, not spec fields.** The v1alpha1 schema stays unchanged, `kubectl annotate` works, the dashboard already writes the annotation, and spec fields can still come in v1beta1. `api/v1alpha1/annotations.go` defines:

- `EnabledAnnotation`, `CommentAnnotation`, `ConditionDisabled` and `ReasonDisabled`
- `IsDisabled(obj)`: true for `"false"`, case-insensitive and trimmed

It is a new file, not a type change, so `make generate` is unaffected.

How the parts treat a disabled object:

- **Policy index (`policy.Build`):**
  - Disabled ClientGroups, Policies and BlockLists are left out as if absent.
  - A reference to one gets a `Disabled` diagnosis instead of `MissingReference`.
  - A disabled list is skipped even while its compiled copy is still in the snapshot.
  - `/info/messages` reports `Disabled` diagnoses as `info`.
- **List resolver and fetcher:** `ZoneCacheObjects.BlockLists()` filters with `listfetcher.EnabledBlockLists`. The leader stops fetching, every node unloads the list, and the cached body is collected once the store is live. Enabling the list fetches it again.
- **Controller:**
  - A disabled object gets Ready False with reason Disabled, Degraded False, and a `Disabled` condition with status True.
  - An enabled object carries no Disabled condition, so existing objects see no status write.
  - A Policy's Ready message names the disabled references it skips.
  - An enabled/disabled flip of a BlockList or ClientGroup requeues the Policies that reference it.

## 4. `/internal/zones` and the zone metrics

- `internalapi.ServerOptions.Zones func(ctx) ZoneList`. The route is registered only when this is set, so older nodes keep answering 404 and `Client.Zones` returns `ErrZonesUnsupported`. The route also gets a metrics label, `zones`.
- `zoneview.Served(cache)` returns the primary and secondary zones of the live view; forward zones are left out, and the result is nil while unsynced.
- `zoneview.Collector` exports:
  - `kudurru_zone_serial{zone}`
  - `kudurru_zone_records{zone,kind}`, where kind is `records` (RR count) or `names` (owner names)

  It reads the snapshot on every scrape.
- serve registers the collector and fills `Zones` from `Served`.

## 5. List refresh and query-log purge

- `listfetcher.Fetcher.RefreshNow(ns, name)` marks the list's URL and ConfigMap sources as forced. `dueAt` returns now for them and ignores the refresh interval and any failure cooldown. It also kicks the run loop. A list that does not exist or is disabled returns `ErrUnknownList`.
- serve's `listRefresher` (`cmd/kudurru/servewire.go`):
  - **Leader:** calls `RefreshNow` and the API answers 202.
  - **Replica:** `api.ErrNotLeader(leader)` gives 409 conflict naming the leader, or 503 while no leader is elected.
  - **Disabled list:** `api.ErrListNotRefreshable` gives 409.
  - The API's refresh handler now passes `*api.Error` values through.
- `sqlitelog.Writer.Purge(ctx, before)` deletes fact rows older than `before` (zero means all) in `DeleteBatch` transactions with an incremental vacuum after each. Rollups and dictionaries are kept. It implements `api.QuerylogPurger`, and serve wires the writer when the sink is sqlite.
- **Smoke run.** `DELETE /querylog` returned `deleted: 1`; `POST .../starter/refresh` returned 202.

## 6. Leader address from the EndpointSlice

- `cluster.EndpointSliceResolve(src, fallback)` finds the leader pod (matched by `targetRef.name` or hostname, ready or not) in the headless Service's EndpointSlices and returns `IP:internalPort`. It falls back to the `<pod>.<headless>` DNS name.
- `ElectorMembership.CacheTTL` (plus a `Now` clock for tests) re-resolves the address periodically. A leader restarted under the same name gets a new IP. serve uses 30 s; zero keeps the old behaviour.
- serve's `leaderResolver`:
  - takes the Service and namespace from `KUDURRU_HEADLESS_SERVICE` (`<svc>.<ns>.svc.<domain>`)
  - uses a discovery/v1 client built from the store's rest config (RBAC already grants it)
  - keeps the DNS name for the sqlite driver, or when the variable is unset
- TLS still verifies the derived internal certificate name, never the address.

## 7. `GET /api/v1/doctor`

`internal/api/doctor.go`, with permission `settings.read` (the same as `/info/messages`). The response is `DoctorReport{node, version, checks:[{check, status ok|warn|fail, message}], failures, warnings}` with these checks:

- **config:** the config source and the settings file.
- **api:** this node answers.
- **token:** the principal and its roles.
- **diagnoses:** `/info/messages` severities.
- **dataDir:** run by the server process. It checks that the directory is writable, not world-writable, and that the databases are not world-readable.
- **ready:** the health checker. Its `dns` component covers bound listeners.

The NOTIFY egress check was not cheap enough to include.

The OpenAPI documents and the generated Go client were regenerated (`make openapi`, `make client-go`). The Tools page still builds its report client-side; switching it to this route is a followups row for the UI owner.

**Finding from the smoke run.** `local.db` and `querylog.db` are created 0644, and doctor warns that they are world-readable. This is a new followups row for the store and querylog owners.

## 8. Drops (ErrDrop)

The row was **corrected**, not merely confirmed:

- `pipeline.ErrDrop` from the control stage used to answer SERVFAIL on every transport.
- The rate-limit drop sent nothing over UDP only, and SERVFAIL elsewhere.

Both now send nothing:

| Transport | Behaviour on a drop |
|---|---|
| udp | writes nothing |
| tcp, dot | the connection is closed (`w.Close()`) |
| doq | the stream is reset with `DOQ_REQUEST_CANCELLED` |
| doh, doh3 | the exchange is aborted (`panic(http.ErrAbortHandler)`): HTTP/2 and HTTP/3 reset the stream, HTTP/1.1 closes the connection. quic-go's http3 server also recovers `ErrAbortHandler` silently. |

- `Handler.Resolve` keeps its never-nil contract and still answers SERVFAIL for drops.
- `resolveFreshID` now returns nil on a drop.
- `TestDropsSendNothing` covers all seven transport variants (both drop forms, then a normal query on a fresh exchange). `TestDropResponse` was updated to the new TCP behaviour.
- The log observer counts drops in `kudurru_dns_responses_total` with `rcode="none"` (`log.RcodeNone`). Plain pipeline errors, which are still answered SERVFAIL, keep `rcode="SERVFAIL"`.

## 9. Controller test flakes

Both fixes address root causes and keep the exact assertions.

- **`TestKubeDriver`:** the controller emits the SerialBumped Event after the status write the test polls for. The test now waits for the Event that carries the final transition.
- **`TestMeasureRecordSetChanges/paced`** had two causes:
  1. **Too few writes (1,871 and 1,982 out of 2,000 under `-cpu 1`).** The loop waited only for the zone's write. The record set's `observedGeneration` write runs on another worker and could slip past the next change of the same object, so two generations shared one write. The loop now also waits for the record set's write, through an `rsWrote` hook channel.
  2. **Too many writes (2,001 to 2,004 under `-race`; the original report).** I traced this with a temporary log of writes and cache upserts:
     1. A zone reconcile writes status A on base RV r, and the cache marks A as pending.
     2. The next reconcile builds B on top of the pending A and writes it, still with base RV r, because the echo of A has not arrived yet.
     3. The echo of A then clears the pending entry, and B is written a second time.

     The sqlite and kube drivers reject step 2 with ErrConflict (the controller requeues). **The memory driver accepted it, because its `UpdateStatus` ignored the caller's resourceVersion.**

     **Fix:** `memory.Store.UpdateStatus` now enforces optimistic concurrency like the other drivers. A new conformance case, `UpdateStatusOptimisticConcurrency` (`internal/store/conformance/status_conflict.go`), pins this for memory, sqlite and kube.

     This is a small edit outside the named packages: the memory driver is the test reference driver, and the fix is parity, not new behaviour.
- **Result after both fixes:** 23 runs passed in total: `-cpu 1,2,4 -count 5` (15) and `-race -cpu 1,2 -count 4` (8).

## 10. sqlite audit

- **Verified:** `WithActor` from the API principal is wired. `api.actorMW` sets `sqlite.WithActor(actorOf(p).String())`, and `TestAuditWithSQLite` asserts that the store-write row names the user.
- **New:** `sqlite.AuditPruner` (400 d retention, daily, runs once at start) runs as the leader-only `audit-prune` runnable for the sqlite driver. It is tested with a fake clock and a verifiable chain afterwards.

## Serve wiring added

`cmd/kudurru/serve.go` keeps its structure; the edits are additive.

- A `config.Reloadable` around `cfg`.
- The `settings` runnable (`WatchSettings`) and the `subscribeReload` targets.
- The logger's `LevelVar`.
- The upstream `Options` are kept for rebuilds, and shutdown closes `fwd.Set()`.
- The limiter and the cache stage are always built.
- `Lameduck: checker.IsLameduck` on the ingress stage.
- `zoneview.NewCollector`, plus `Zones` on the internal server.
- `leaderResolver` with a 30 s `CacheTTL`.
- API options: `Reload`, `Settings`, `Upstreams` (func), `Refresher`, `Purger`.
- The `audit-prune` leader runnable.
- `main.go` registers `newLameduckCmd`.

New files:

- `cmd/kudurru/reload.go`: the subscribers and the settings writer.
- `cmd/kudurru/servewire.go`: the refresher, purger and leader resolver.
- `cmd/kudurru/lameduck.go`

`internal/config`: the `settingsFile` key, the settings layer in `Load` and `reload.go`. The edits are additive and the drift test is green.

## Architecture changes (for the integrator's decision log)

- **Settings file layer (implements D25 in the binary):** the order is defaults, node file, settings file, environment. The file holds cluster-scope keys only and is watched with fsnotify plus a 10 s poll. A PATCH persists into the file in Docker and answers 409 managed_by in Kubernetes. Node-scope PATCHes stay in memory. `restartRequired` is enforced by `TestRestartRequired`.
- **`kudurru.io/enabled: "false"`** as the v0.1 switch for BlockList, ClientGroup and Policy, with a Disabled condition. Rejected: `spec.enabled` and `spec.comment` in v1alpha1.
- **Drops send nothing on every transport.** tcp and dot close, doq resets, doh and doh3 abort; the rcode label is `none`. This replaces "SERVFAIL over TCP".
- **Leader address from the EndpointSlice,** with the DNS name as fallback and a 30 s address TTL.
- **`/lameduck` on the ops listener, loopback only.** Rejected: an admin-socket verb (adminsock lives in the auth package) and an unauthenticated remote POST.

## Followups

Opened in docs/followups.md:

- **Chart:** set `settingsFile`, drop the settings merge, and use `preStop: kudurru lameduck`.
- **Compose:** set `KUDURRU_SETTINGS_FILE`.
- **Store and querylog owners:** create `local.db` and `querylog.db` with mode 0600.
- **UI owner:** the Tools page reads `/doctor`.
- **Deploy owner:** add the serial-divergence panel and alert, now that the metric exists.
- **Reload caveats:** the pseudonym key rotates when privacy settings reload, and the cache keeps its shard count.
- **Node-scope persistence:** `PATCH /node/config` changes are not persisted.
