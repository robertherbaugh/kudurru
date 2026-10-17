# M1 work package: zonecache

Branch `worktree-wf_0eb072cf-675-3`. Covers dev-docs/design-record.md §3.1 (zonecache row), §4.4 (cold start without the apiserver), §4.5, the §5.2 readiness probe, and decisions D7, D33 and D78.

## What was built

`internal/zonecache` is the always-on runnable that turns store watches into serving snapshots and keeps `local.db` current.

- **Watches.** One `Store.Watch` per kudurru.io kind (`DefaultKinds()` reads them from the v1alpha1 scheme, so a new kind is picked up without touching zonecache). It follows the store's Watch contract: initial `Added` events, then a `Bookmark`, then live events. When a watch channel closes, the kind is re-listed with a new Watch. Objects missing from the new list are dropped when its Bookmark arrives, and `kudurru_store_watch_resyncs_total{kind}` goes up by one. `Watch` errors are retried with backoff from 100 ms to 5 s. `store.ErrClosed` ends that kind's watch.
- **Indexed object set (`ObjectSet`).** Immutable. Lookups by key, sorted per-kind lists, a label index (`WithLabel`, `Select` with equality selectors), DNSRecordSets grouped by `spec.zoneRef.name` (`RecordSetsForZone`, `ZoneRefs`), and typed accessors (`Zones`, `Zone`, `RecordSets`, `Policies`, `ClientGroups`, `BlockLists`). It also carries the compiled blocking lists (`Lists()`, set through `Cache.SetLists`), the build time (`Now()`), per-kind change flags (`Changed(kind)`, `ListsChanged()`) and per-kind resourceVersions. Kinds that did not change share their index with the previous set, so editing one zone re-indexes only DNSZone.
- **Snapshots.** After a change, a 50 ms window collects further changes. Then the builders run in registration order and one `*Snapshot` is published through an `atomic.Pointer`. The query path calls `Cache.Snapshot()`: one atomic load, no lock, no allocation (9.3 ns including a `View` lookup). Record `expiresAt` values schedule a rebuild when they pass, so builders that honour expiry (§4.2) see the change without a store event.
- **Builder isolation.** If a builder returns an error or panics (the panic is recovered), its previous view stays in service. The other builders are unaffected. The failure raises `Degraded{BuildFailed, builder}` and increments `kudurru_zonecache_build_errors_total{builder}`. The condition clears on that builder's next successful build.
- **Sync state and readiness.** `State()` returns `Unsynced`, `FromSnapshot` or `Live`. `Synced()` is true once every kind has delivered its initial list and Bookmark. `Ready() (bool, []string)` matches `health.ReadyFunc` and implements the §5.2 rule: ready when live, or when serving a snapshot younger than `MaxAge`. When the Cache is ready, the strings list any active Degraded conditions so `/healthz` shows them. When it is not ready, they say why ("waiting for the initial sync of DNSZone, Policy", "local.db holds no snapshot yet", "serving a local.db snapshot 25h0m0s old, older than maxAge 24h0m0s"). `Initialized()` is closed once the Cache has something to serve: the store synced or `local.db` was loaded.
- **Cold start (§4.4).** With `Options.LocalDB` set:
  - The live set and per-kind resourceVersions are written to `local.db` after every successful sync or re-sync, then at most every `PersistInterval` (30 s) when something changed, and once more on shutdown.
  - If the store has not synced within `StartupTimeout` (15 s), `local.db` is loaded and served. State is `FromSnapshot`, with `Degraded{ServingFromSnapshot}`, `kudurru_store_snapshot_serving 1` and `kudurru_store_snapshot_age_seconds`.
  - If the snapshot is older than `MaxAge` (24 h), or crosses it while being served (a timer republishes the snapshot), `Snapshot.Stale` is set, `Snapshot.Authoritative()` returns false and `Ready()` fails.
  - When the store syncs, the live set replaces the snapshot. Nothing is written back while the snapshot is being served.
  - Unsynced with no snapshot: not ready, and `Authoritative()` is false.
- **`local.db` moved** from `internal/store/kube/localdb` to `internal/store/localdb`. zonecache is driver-independent and is the only writer, so it made sense to move it. The kube package now imports the new path (three files). Table names and the on-disk schema are unchanged. A new `SnapshotStore.Touch(ctx, t)` updates `written_at` without rewriting any objects (see "Surprises").

## The Builder contract

```go
type Builder interface {
    Name() string                          // stable, unique; keys the view and the metric label
    Build(objects ObjectSet) (any, error)  // returns an immutable view
}
func NewBuilder(name string, build func(ObjectSet) (any, error)) Builder
func View[T any](s *Snapshot, name string) (T, bool)
```

- `Register` rejects duplicate and empty names. You can register builders before or after `Start`. Registering one triggers a rebuild.
- `Build` runs on the Cache's single rebuild goroutine, never at the same time as itself or another builder. It runs once per published snapshot: after the debounce window that follows a store change, on `SetLists`, when the source switches (Unsynced, FromSnapshot, Live), when a record's `expiresAt` passes, when a builder is registered, and when a served snapshot crosses `MaxAge`.
- The `ObjectSet` and every object reachable from it are shared and immutable. `Build` must not modify them, but it may keep references to them. Returned slices are clipped, so `append` copies.
- The returned value goes to every reader exactly as returned. It must be safe for concurrent reads and must never be modified afterwards (copy-on-write). To reuse unchanged parts of the previous view, check `objects.Changed(kind)` and `objects.ListsChanged()`. On the first build and on a source switch, both report true for everything.
- Use `objects.Now()` for time comparisons such as `expiresAt`, not the wall clock. That way every view in a snapshot agrees.
- Error or panic: the previous view is kept and `Degraded{BuildFailed}` is raised. A builder that has never succeeded has no view (`View` returns `ok=false`).
- `Build` should take milliseconds. It delays the builders after it and the publication of the snapshot. It must not block on the Cache.

## Wiring (`cmd/kudurru serve`)

```go
// kube mode only: Docker mode has state.db locally and passes LocalDB nil (§4.5).
db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(cfg.Node.DataDir, localdb.FileName))
if err != nil { return err }            // ErrSchemaTooNew: refuse to start (§4.3)
defer db.Close()                          // after zc.Start has returned

zc, err := zonecache.New(st, zonecache.Options{
    LocalDB:        db,                    // nil in Docker/sqlite mode
    StartupTimeout: 15 * time.Second,      // store.kube.startupTimeout (no config key yet)
    MaxAge:         24 * time.Hour,        // store.kube.snapshot.maxAge (no config key yet)
    Registerer:     reg,
    Logger:         logger,
})
if err != nil { return err }
_ = zc.Register(zoneBuilder)                  // from internal/zone (parallel package)
_ = zc.Register(policyIndexBuilder)           // from the policy index
checker.Register("zonecache", zc.Ready)
run("zonecache", zc.Start)                     // blocks until ctx ends; final local.db write
// startup probe: SetStarted only after <-zc.Initialized() (and listeners bound)
// list refresher: zc.SetLists(blockingSnapshot) after every blocking.Rebuild
```

Query path (the authoritative stage):

```go
snap := zc.Snapshot()                              // once per query
if inPrimaryZone && !snap.Authoritative() { return SERVFAIL }   // Unsynced, or snapshot older than maxAge
zones, ok := zonecache.View[*zone.Set](snap, "zones")
```

Notes:
- `Cache` implements controller-runtime's `LeaderElectionRunnable` (`NeedLeaderElection() == false`) and `Start(ctx) error`, so the future manager can run it unchanged.
- With zonecache running, the kube driver's `WaitForSync`, `MaterializedSnapshot` and `PersistSnapshots` are no longer needed on the serve path. zonecache waits on its own Bookmarks and writes its own set. Their `StartupTimeout` options are independent.
- `Snapshot.State` and `Authoritative()` lag `Cache.State()` by one debounce window. The query path should use the snapshot.

## Public API summary

- `New(store.Store, Options) (*Cache, error)`, `DefaultKinds() []store.Kind`, the `Default*` constants (Debounce 50 ms, PersistInterval 30 s, RefreshInterval 5 min, StartupTimeout 15 s, MaxAge 24 h)
- `(*Cache)`: `Register(Builder) error`, `SetLists(*blocking.Snapshot)`, `OnPublish(func(*Snapshot))`, `Start(ctx) error`, `Snapshot() *Snapshot`, `State() State`, `Synced() bool`, `Ready() (bool, []string)`, `Initialized() <-chan struct{}`, `Degraded() []Condition`, `NeedLeaderElection() bool`
- `Snapshot`: `Generation`, `State`, `Objects ObjectSet`, `BuiltAt`, `SnapshotWrittenAt`, `Stale`, `View(name)`, `Authoritative()`, `Degraded()`
- `ObjectSet`: `Kinds`, `Len`, `Count`, `Get`, `List`, `WithLabel`, `Select`, `Zones`, `Zone`, `RecordSets`, `RecordSetsForZone`, `ZoneRefs`, `Policies`, `ClientGroups`, `BlockLists`, `Lists`, `Now`, `Changed`, `ListsChanged`, `ResourceVersion`
- `Condition{Reason, Builder, Message, Since}` with reasons `ServingFromSnapshot`, `SnapshotStale`, `BuildFailed`, `SnapshotWriteFailed`
- Metrics: `kudurru_store_watch_resyncs_total{kind}`, `kudurru_zonecache_rebuilds_total`, `kudurru_zonecache_rebuild_duration_seconds`, `kudurru_zonecache_build_errors_total{builder}`, `kudurru_zonecache_objects{kind}`, `kudurru_store_snapshot_serving`, `kudurru_store_snapshot_age_seconds` (age of the served snapshot, 0 when live), `kudurru_store_snapshot_writes_total{result}`, `kudurru_store_snapshot_last_write_timestamp_seconds`

## Measurements

Shared 8-CPU sandbox, `go test` without `-race` unless noted (`TestRebuild10k`, benchmarks in `scale_test.go`). The 10k set is 50 zones, 9,500 record sets with two labels each, 200 policies, 150 client groups and 100 block lists.

| What | Result |
|---|---|
| 10k objects, start to first Live snapshot (includes the 50 ms debounce) | 65 ms (187 ms with `-race`) |
| 10k objects, one record-set change until it is visible | 61 ms (50 ms of that is debounce) |
| Full index build, 10k objects (`BenchmarkIndexFull10k`) | 7.4 ms, 2.4 MB, 2,464 allocs |
| Re-index after one zone edit (`BenchmarkIndexOneZone10k`) | 5.2 µs, 11 allocs |
| Query path: `Snapshot()` + `Authoritative()` + `View` | 9.3 ns, 0 allocs |
| `local.db` write, 10k objects | 136 ms (on the persist goroutine, off the rebuild path) |
| `local.db` load, 10k objects | 54 ms |
| envtest: restarted replica, apiserver partitioned, `StartupTimeout` 1 s | fell back to `local.db` after 1.02 s |
| envtest: partition healed until swapped to live | 392 ms |
| `go test -race ./internal/zonecache/` | 12-20 s (the envtest case is about 7 s) |

## Tests

- **Memory driver:** live sync and every `ObjectSet` accessor; live update, add and delete; unchanged kinds share their index; debounce (a burst is one rebuild, two spaced changes are two, nothing visible before the window ends); builder error and panic isolation and recovery; a builder that never succeeds; late registration plus `SetLists`; a rebuild triggered by `expiresAt`.
- **Fake store** (unbounded watch buffers, held Bookmarks, forced channel close, events that bypass watchers): re-list after a watch closes, including a delete and an add the watch never reported; `Watch` errors retried; deletes during the initial list; persist, then cold start, then swap; stale snapshot; a snapshot crossing `MaxAge` while served; unsynced with an empty `local.db` and with no `local.db`; timestamp refresh; final write on shutdown; write failure raising `SnapshotWriteFailed` without costing readiness; 10k timing.
- **Kube driver on envtest** (`TestKubeColdStart`, skipped when `KUBEBUILDER_ASSETS` is unset or `KUDURRU_SKIP_ENVTEST` is set): a live Cache writes `local.db` with every object, every kind's resourceVersion and the kube-system UID. A restarted replica behind a failing RoundTripper falls back after `StartupTimeout` and builds its views from the snapshot, with the age gauge set. A write is made while it is partitioned. When the partition heals, the replica swaps to the live set, including that write, and persists it.

## Surprises

1. **A quiet cluster would age its own snapshot past `maxAge`.** If `local.db` is written only when something changes, a cluster with no edits for a day has a snapshot more than 24 h old, and a pod restart would then answer SERVFAIL for primary zones. That defeats D7 on exactly the homelab installs it targets. zonecache therefore refreshes `written_at` (`SnapshotStore.Touch`) every `RefreshInterval` (5 min) while live and unchanged. The age then means "time since the set was last known current".
   - Caveat: the kube driver keeps serving from informer memory during an apiserver outage and gives no "connected" signal, so the Cache stays `Live` and keeps refreshing. The age after a restart therefore measures time since the pod last ran, not time since it last reached the apiserver. See follow-ups.
2. **The memory driver deadlocks `Watch` with more than 256 objects.** It sends the initial `Added` events into a 256-slot channel while holding its mutex, before returning the watcher. Live events beyond the buffer are silently dropped for slow watchers. The zonecache tests use a fake store for the 10k and resync cases instead of changing a shared package.
3. **`metav1.Time` serialises with second precision**, so `expiresAt` values written through any store lose sub-second precision. That matters only for tests.
4. The kube driver already had `MaterializedSnapshot` and `PersistSnapshots`, which write from the informer cache. zonecache writes its own object set instead, so the snapshot and the views come from the same events and the logic works with any driver. The kube helpers remain for the driver's own tests.

## Follow-ups

- **Config keys** (owner of `internal/config`): `store.kube.startupTimeout` (15 s) and `store.kube.snapshot.maxAge` (24 h) are named in §4.4 but not in `schema.go`. Wire them into `zonecache.Options`.
- **Store connectivity signal:** expose the kube driver's last successful contact (for example `store.Info().LastContact`, or a `Connected()` method) so zonecache stops refreshing the snapshot timestamp during an apiserver outage.
- **Memory driver:** deliver initial `Watch` events from a goroutine and stop dropping events for slow watchers, or document both limits on `memory.Store`.
- **`/healthz` `snapshotAge`:** §5.2 lists `snapshotAge` in the roll-up. `health.Checker`'s info map is static. A dynamic info hook (or zonecache's ready message, which already contains the age) would fill it.
- The cold-start check does not compare cluster IDs (the snapshot records the kube-system UID, but the apiserver is unreachable at that point). A `local.db` copied from another cluster would be served until the store syncs.
- §5.2 startup probe: `serve` should gate `SetStarted` on `<-zc.Initialized()`. That change is in `cmd/`, which this package does not touch.

## Recommended changes to dev-docs/design-record.md

- §4.4: "writes ... after every successful sync and then at most every 30 s when changed" should add: "and refreshes the snapshot timestamp every 5 min while unchanged, so `snapshot_age` measures time since the set was last known current". Also define `kudurru_store_snapshot_age_seconds` as 0 while serving live objects; the `KudurruSnapshotStale` alert depends on this.
- §4.4: the startup timeout applies to every driver that is given a `local.db`, not only the kube driver's informers. zonecache measures it against its own watch Bookmarks.
- §4.5 / §12 layout: `local.db` code lives in `internal/store/localdb`. It is the natural single owner of `local.db` migrations, which the kube spike asked for in its recommendation 7.
- §3.1 zonecache row: mention the `Builder` seam (`internal/zonecache.Builder`) as the plug-in point for the zone trees, the policy index and later views.
