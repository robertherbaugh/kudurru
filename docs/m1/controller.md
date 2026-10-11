# M1 work package: controller (Lease-elected controller and cluster runner)

Branch `worktree-wf_0eb072cf-675-5`. Packages: `internal/cluster`,
`internal/controller`, `internal/controller/kubeevents`; contract changes in
`internal/store` (Elector), `internal/store/kube`, `internal/store/memory`.

## What was built

### `store.Elector` signature (D78), implemented

```go
type Elector interface {
    Run(ctx context.Context, onStarted func(term context.Context), onStopped func()) error
    IsLeader() bool
    Leader() string
}
```

`onStarted` receives the term context (cancelled on leadership loss or when
`ctx` ends) and may block until it is done; `onStopped` runs after `onStarted`
returned and the term context is done; terms never overlap; `Run` returns
only when `ctx` is cancelled. The kube `LeaseElector` already had these
semantics as `RunWithContext`; it is now `Run` (nil callbacks allowed,
`RunWithContext` removed). The memory driver passes its context as the one
term. All kube elector tests (envtest) and the conformance suite pass
unchanged apart from the callback type at two call sites.

**Merge note:** any other driver implementing `store.Elector` on a parallel
branch (the sqlite driver's always-leader elector) must take
`func(context.Context)` for `onStarted`: call `onStarted(ctx)` then
`<-ctx.Done()` then `onStopped()`. It is a compile error, not a silent
behaviour change, if missed.

### `internal/cluster.Runner`

- `New(Options) (*Runner, error)`, `Add(name string, leaderOnly bool, fn RunFunc) error`, `Run(ctx) error`, `IsLeader()`, `Terms()`, `Status() []RunnableStatus`.
- Always-on runnables start at once. Leader-only runnables start as a group
  inside `onStarted(term)` and are cancelled with the term. `onStarted`
  blocks until every leader runnable has returned, so the elector's
  "no overlapping terms" guarantee covers the work, not just the callback.
- Leadership loss never ends `Run`. A failing or panicking leader-only
  runnable restarts with exponential backoff (1 s to 30 s) inside the term;
  an always-on failure is fatal (ordered shutdown, `Run` returns the error),
  matching today's serve behaviour for bind errors. An elector whose `Run`
  fails (invalid lease name) is fatal too.
- Without leader-only runnables the Runner never campaigns (no Lease).
- Shutdown order: `Options.Lameduck(ctx)` then `LameduckDuration`, then the
  election context is cancelled (the elector ends the term, waits for the
  group, then releases the Lease), then always-on runnables are cancelled.
  Each phase is bounded by `ShutdownTimeout` (30 s) and logs stragglers.
- Gauges `kudurru_leader{role="<election>"}` and
  `kudurru_role_active{role="<runnable>"}`.

### `internal/controller`

One reconciler per kind, fed by `Store.Watch` into a watch-fed cache (the
reconcilers never read the store) and five rate-limited client-go
workqueues (`DefaultTypedControllerRateLimiter`, 2 workers per kind).
Workers start only after every kind's initial list (first bookmark), so no
reconcile acts on a partial view. Every 10 minutes each kind's watch is
re-opened: its initial list prunes objects deleted while unobserved and
re-queues every object. Each reconcile computes the whole status from
current state and calls `UpdateStatus` only if it differs
(`equality.Semantic.DeepEqual`; `meta.SetStatusCondition` keeps
`lastTransitionTime`). A write is recorded as pending until the watch
echoes it, so a reconcile in between compares against what was written,
not the stale cache. Writes carry the cached resourceVersion, so the kube
driver applies optimistic concurrency; conflicts are re-queued.

| Kind | Status written | Conditions |
|---|---|---|
| DNSZone | observedGeneration, recordCount (RRs = targets), contentHash, serial; `published` untouched (empty) | Ready: Serving / Invalid (bad recordSetSelector) / Unsupported (types other than primary, forward). Degraded summarises record sets not fully served (reason priority Duplicated, OutsideZone, Invalid, NotSelected) |
| DNSRecordSet | observedGeneration, names | Ready=False ZoneNotFound / NotSelected / Invalid (whole object excluded); Degraded=True OutsideZone / Duplicated (endpoints dropped; Ready stays True while anything is served) |
| BlockList | observedGeneration | Ready Serving; Degraded ListFetchFailed from `status.sources[].error` |
| ClientGroup | observedGeneration, members | Ready Serving |
| Policy | observedGeneration | Ready Serving; Degraded MissingReference naming each missing ClientGroup/BlockList (same namespace) |

Zone model (`zoneview.go`, a pure function with table tests): record sets
are ordered by creationTimestamp, then namespace/name; the identity is
(lower-case FQDN, TYPE, setIdentifier); the earlier object keeps a contested
identity and the later gets Duplicated (disabled endpoints still hold their
identity so toggling never moves ownership); TTL is recordTTL, else the
zone's defaultTTL, else 300. The **serial** is stamped only when
contentHash changes: `NextSerial(prev, now) = max(prev+1, unix_now)` in
RFC 1982 arithmetic (D12). Zone reconciles caused by record-set changes,
and the echo of the zone's own status write, are delayed by
`ZoneCoalesce` (250 ms): client-go's delaying queue keeps the earliest
time, so a burst is stamped as one serial per window.

**contentHash** lives alone in `contenthash.go`: sha256 over sorted lines of
Go-quoted `(name, type, setIdentifier, ttl, sorted targets)`. internal/zone
owns the canonical RR form ("sha256 over sorted canonical RRs"); swapping
it in means replacing `ContentHash` and the tuple canonicalisation in
`buildZoneView`, which stamps one new serial per zone on upgrade.
Endpoint parsing goes through the `Options.Validate` seam
(`EndpointValidator`); the default `ValidateEndpoint` checks names and
A/AAAA/CNAME/DNAME/NS/PTR/MX/SRV/CAA presentation format without
miekg/dns (§2).

Events: `EventSink` interface (`Event(obj, type, reason, message)`),
`LogSink` (default), `MemorySink` (tests), `MultiSink`. SerialBumped
(Normal) after a stamped write; Duplicated, OutsideZone, Invalid (Warning)
when the problem's text is new on that object, never on every reconcile.

`internal/controller/kubeevents.Sink` is a client-go `record` broadcaster
writing core/v1 Events (`New(typedcorev1.EventsGetter, Options)` or
`NewForConfig(*rest.Config, Options)`, `Shutdown()`). The controller
package never imports it. Events for DNSZone (cluster-scoped) land in
`default`.

Metrics: `kudurru_controller_reconciles_total{kind,result}`,
`kudurru_controller_reconcile_duration_seconds{kind}`,
`kudurru_controller_status_writes_total{kind}`,
`kudurru_controller_resyncs_total{kind}`.

### Memory driver fix

`memory.Store.Watch` queued all initial objects into a 256-slot channel
under the store lock, so watching a kind with more than 256 objects
deadlocked; the buffer is now sized to the initial set plus 257
(`TestWatchManyInitialObjects`).

## Wiring for `kudurru serve`

```go
runner, err := cluster.New(cluster.Options{
    Elector:          st.Elector(cluster.ElectionController), // Lease kudurru-controller (kube), always-leader (sqlite)
    Lameduck:         func(context.Context) { checker.Lameduck() },
    LameduckDuration: cfg.Lameduck,            // e.g. 5 s; config key to add in internal/config
    Registerer:       reg,
    Logger:           logger,
})

var sink controller.EventSink = controller.LogSink{Logger: logger}
if kubeStore != nil {                          // the kube driver is in use
    ks, err := kubeevents.NewForConfig(restCfg, kubeevents.Options{})  // same rest.Config as kube.New
    if err != nil { return err }
    defer ks.Shutdown()
    sink = controller.MultiSink(ks, sink)
}
ctrl, err := controller.New(st, controller.Options{Events: sink, Registerer: reg, Logger: logger})

runner.Add("dns", false, registry.Run)
runner.Add("ops", false, opsServer)
runner.Add("metrics", false, metricsServer)
runner.Add("selfcheck", false, selfcheck)
runner.Add("zonecache", false, zc.Run)        // when it lands
runner.Add("controller", true, ctrl.Run)       // leader-only
// later leader-only: "listfetcher", "notifier"; always-on: "api", "querylog", "internal"
return runner.Run(ctx)                         // replaces the hand-rolled errCh/WaitGroup in serve.go
```

The query-log flush (`collector.Close`) stays after `runner.Run` returns.
Kube driver: `kube.New(restCfg, kube.Options{...})` then `WaitForSync`
(fall back to local.db per §4.4); the controller needs no extra RBAC beyond
D77 (`*/status` patch/update, `events` create/patch, leases).

## Measurements

8 shared CPUs, no `-race`, Go 1.27.2. Memory driver unless stated.

| Scenario | Result |
|---|---|
| 1,000 record-set creates, back to back, ZoneCoalesce off | 1,022 status writes (1,000 DNSRecordSet + 22 DNSZone) = **1.02 per change**; zone converged 39 ms after the first apply |
| same, ZoneCoalesce 250 ms (default) | 1,001 writes = **1.001 per change**, **1 serial bump**; converged after ~200-250 ms |
| 1,000 record-set updates, paced (each converges before the next) | **2.000 writes per change** (record set observedGeneration + zone serial); end-to-end Apply → zone status written p50 0.41-0.53 ms, p90 0.73 ms, p99 1.1 ms |
| reconcile latency per event (paced) | DNSRecordSet p50 54-106 µs, p99 0.69-0.75 ms; DNSZone (100 record sets) p50 55-204 µs, p99 0.71-0.77 ms. No-op reconciles are ~40 µs; writes are dominated by the memory driver's JSON round trip |
| new term (or resync) over 1,000 converged record sets | all 1,001 objects reconciled in 11-13 ms, **0 status writes**, serial unchanged |
| kube driver (envtest), 300 creates back to back, default coalescing | applied in ~1 s; zone converged 1.2 s after the first apply; 305 status writes = **1.017 per change** (5 zone writes), 0 conflicts. Without coalescing: 584 writes, 284 serial bumps, 26-40 conflicts retried. Reconcile p50 1.2 ms (record set), 2.8 ms (zone): one apiserver round trip |
| Runner on kube LeaseElector (envtest, 2 s / 1.5 s / 200 ms) | graceful leader stop → successor's leader group running in **156 ms** (lease released after the group stopped); stolen lease → old leader's group stopped after RenewDeadline with the Runner and always-on runnables still running |

Reproduce: `go test -run TestMeasure -v ./internal/controller/` (memory);
add `KUDURRU_MEASURE=1 KUBEBUILDER_ASSETS=...` for the kube run.

## Tests

- `internal/cluster`: fake elector (grant/revoke) for handoff over three
  terms, shutdown order, restart with backoff and panic recovery, fatal
  always-on failure, fatal elector error, no campaign without leader-only
  runnables, the memory elector; `TestLeaseHandoff` runs two Runners on one
  Lease in envtest (follower never starts the group, stolen lease stops only
  the group, re-acquisition restarts it, graceful handoff without overlap).
- `internal/controller` (memory driver unless noted): zone status, serial
  rule (no bump on re-apply, label-only change or target reorder; prev+1
  within a second; unix_now after the clock moves; monotonic when the clock
  steps back; bump on a defaultTTL change), duplicate ordering independent
  of write order and its clearing, OutsideZone / Invalid / ZoneNotFound
  table, policy references across create/delete and namespaces, BlockList
  fetch errors preserved alongside listfetcher fields, status write
  suppression through 10+ resyncs (UpdateStatus counted by a wrapping
  store), resync repairing hidden add and delete events, retries on errors
  and conflicts, idempotent new term, zone coalescing, table tests for the
  zone model, `ContentHash` (order independence, field sensitivity, golden),
  `NextSerial`, `ValidateEndpoint`. `TestKubeDriver` runs the main paths on
  envtest (CRD status validation, optimistic concurrency, no write loops).
- `internal/controller/kubeevents`: Events land on the fake clientset with
  the right involvedObject, namespace (`default` for DNSZone) and source.

## Surprises

- The kube `LeaseElector` already implemented the D78 shape as
  `RunWithContext`; the change was a rename plus the interface.
- The memory driver's Watch deadlocked above 256 objects of a kind (fixed).
- Without coalescing, a bulk import against the apiserver stamps one serial
  per record set (284 bumps for 300 objects in a second) and the echo of
  each zone write re-triggers the zone. D12 says "whenever contentHash
  changes"; coalescing over 250 ms keeps that rule while making the serial
  move once per burst.
- `metav1.Condition` needs a reason for False conditions; the API types had
  no "nothing wrong" reason for `Degraded=False`.

## Follow-ups and recommended architecture changes

- **api/v1alpha1**: add the reasons the controller defines locally
  (`AsExpected`, `ZoneNotFound`, `NotSelected`, `MissingReference`,
  `Unsupported`) to `common_types.go`.
- **docs/architecture.md §4.3**: replace the Elector line with the new
  signature and the term contract above; §5.3: note the Runner's shutdown
  order and that leader-only runnables restart within a term instead of
  ending it. D12: add "reconciles triggered by record-set changes coalesce
  over 250 ms (one serial per burst)".
- **internal/zone**: swap `ContentHash`/tuple canonicalisation for the
  canonical RR form and plug its parser into `Options.Validate`; add CNAME
  exclusivity (CNAME plus other data at one name) to the zone model, which
  currently only detects same-identity duplicates.
- **listfetcher**: write `status.sources`, entries and digests only and
  leave Ready/Degraded to the controller (which reads `sources[].error`), or
  agree on a separate condition type; with the sqlite driver (no
  resourceVersion check on UpdateStatus) two writers of one status would
  otherwise overwrite each other.
- `expiresAt` is honoured by zonecache only; the zone hash ignores it, so an
  expiring record does not bump the serial. A per-zone timer requeue at the
  next expiry would fix that if secondaries need it.
- ClientGroup `sources.integrationRef` and BlockList `configMapRef` are not
  validated (Integration is v0.2; ConfigMaps are outside the Store).
- `internal/config`: a `lameduck` duration key for `Options.LameduckDuration`.
- Per-instance integration Leases (v0.2) can run as additional Runners (one
  per elector name) or as a Runner method that takes an elector per
  runnable.
- NOTICE: the existing client-go / controller-runtime entries sit under the
  "Test data" heading although they are linked dependencies.
