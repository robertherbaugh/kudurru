# Spike (c): the `kube` Store driver on envtest

Key `store-kube`, week 1. Scope: architecture §4.3 (Store interface, Apply contract), §4.4 (cold start without the apiserver), §4.5, §5.1, §5.3 (D44 Lease election), decisions D2, D7, D9, D10, D33.

## Verdict

| Gate | Result |
|---|---|
| All conformance cases pass | **Partial: 6 of 7 pass unchanged.** `ApplyOptimisticConcurrency` cannot pass against a real apiserver. Re-applying an identical object is not a write there, so neither `resourceVersion` nor `generation` changes. The exact suite change is below. It passes on both `kube` and `memory` (`TestApplyOptimisticConcurrencyWithSpecChange`). |
| The elector never returns from Run on lease loss | **Pass.** Covered by four envtest scenarios (lease expiry under partition, lease deleted, lease hijacked, panic in leader work) and once with the production 15 s / 10 s / 2 s timings. In every case `onStopped` fired, `Run` kept running, the other candidate took over, and terms never overlapped. |

Overall: **partial**. The driver, the elector and the local.db proof are complete and green. One suite case needs the one-line change proposed below, which the suite owner has to apply. Two bugs outside this scope were found and documented with exact fixes: the generated BlockList and ClientGroup CRDs are rejected by Kubernetes 1.37, and the §5.3 ClusterRole lacks the write verbs the driver needs.

## What was built

All code is in `internal/store/kube` (about 2,300 lines of code and 2,500 lines of tests). Other changes: `go.mod`/`go.sum`, `NOTICE`, and a `make envtest` target.

| File | Content |
|---|---|
| `kube.go` | `Options`, `New(cfg, opts)`, `WaitForSync`, `Info`, `Close`. Builds an uncached controller-runtime client and an informer cache (one informer per kind, `managedFields` stripped) on a shared HTTP client and a lazy dynamic RESTMapper. `New` does no blocking I/O, so it succeeds with the apiserver down. Unset client QPS becomes 50/100 instead of client-go's 5/10. |
| `kinds.go` | Kind registry derived from the scheme. Every kudurru.io type with a `<Kind>List` is served, so a new kind is picked up with no driver change. List types are recognised by type, not by name, because `BlockList` is a kind. |
| `crud.go` | `Get`/`List`: consistent reads from the apiserver, with label selectors and keyset pagination through `limit`/`continue`. `Apply`: server-side apply with `FieldOwner(manager)` and `ForceOwnership` only on Force; see the Apply section. `Delete`: honours managedBy, atomic through UID and resourceVersion preconditions, finalizers honoured by the apiserver. `UpdateStatus`: status subresource, with optimistic concurrency when the caller passes a resourceVersion and a retry loop when it does not. |
| `watch.go` | `Watch` over the shared informer. Initial state arrives as `Added`, then one `Bookmark` whose object carries the informer's last-synced resourceVersion, then live `Added`/`Modified`/`Deleted` filtered by namespace. Resyncs are dropped and tombstones unwrapped. A catch-up barrier makes the initial list include every write that completed before `Watch` was called. |
| `snapshot.go` | `Snapshot`: `kind: List` YAML of every kind, paged from the apiserver, `managedFields` omitted. `Restore`: apply with each object's recorded owner. `MaterializedSnapshot`: the informer set plus per-kind resourceVersions. `PersistSnapshots`: writes local.db after sync, then at most every interval when something changed. |
| `elector.go` | `LeaseElector`, the `store.Elector` (D44 semantics). `Store.Elector(name)` maps names to Lease `kudurru-<name>`. Also `RunWithContext(ctx, onStarted(termCtx), onStopped)`. |
| `localdb/localdb.go` | `SnapshotStore` on modernc.org/sqlite (`local.db`, WAL), with the shared `meta.schema_version` check (`ErrSchemaTooNew`). The tables are `kube_snapshot`, `kube_snapshot_kinds` and `kube_snapshot_objects`. Readable without any Kubernetes client. |

### Apply in Kubernetes (D9)

1. A metadata-only GET, then the managedBy pre-check: annotation owner ≠ `Manager` and not Force gives `*store.ErrManagedBy`. A stale `ExpectedResourceVersion`, or one naming a missing object, gives `store.ErrConflict`.
2. The SSA body is the whole object minus `status` and server-managed metadata, with `kudurru.io/managed-by` set. The resourceVersion observed in step 1 is sent as a precondition, so the check and the write are atomic: if the object changed in between, step 1 is repeated, up to 5 times. Concurrent first writers are serialised by SSA ownership of the annotation field. `TestApplyConcurrentFirstWriters`: 8 managers race, exactly one wins, and the other 7 get `ErrManagedBy` naming the winner.
3. A field-manager conflict (HTTP 409 with `FieldManagerConflict` causes, for example fields owned by `kubectl apply --server-side`) is reported as `ErrManagedBy`. The owner is the managed-by annotation if it names someone else, otherwise the conflicting field manager.
4. **Force is a whole-object takeover.** Plain `ForceOwnership` only takes the fields the new manager sends. Fields set only by the previous owner or by kubectl would stay, unlike in the reference driver. Before a forced apply the driver therefore reassigns every other manager's main-resource field ownership to the new manager, using a merge patch on `managedFields` guarded by resourceVersion. The forced apply then removes whatever the new manager leaves out. Status ownership is untouched. Covered by `TestForceIsWholeObjectTakeover`.
5. Managers longer than 128 characters or containing non-printable characters (the apiserver's `fieldManager` limits) map to a readable prefix plus a hash suffix. The annotation always holds the full string.

## Conformance on envtest

The suite (`internal/store/conformance`) is unchanged. The factory gives each case an empty control plane (all kudurru.io objects deleted) and a fresh driver.

```
$ make envtest
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.37.x --bin-dir .../bin -p path
.../bin/k8s/1.37.0-linux-amd64

$ KUDURRU_CONFORMANCE_STRICT=1 go test -count=1 -run TestConformance -v ./internal/store/kube/
=== RUN   TestConformance/ApplyOptimisticConcurrency
    conformance.go:89: resourceVersion must change on every write
--- FAIL: TestConformance (5.58s)
    --- PASS: TestConformance/ApplyCreatesWithManagedBy (5.04s)
    --- PASS: TestConformance/ApplyRejectsOtherManager (0.02s)
    --- FAIL: TestConformance/ApplyOptimisticConcurrency (0.10s)
    --- PASS: TestConformance/DeleteAndNotFound (0.10s)
    --- PASS: TestConformance/ListFiltersByKindNamespaceLabels (0.18s)
    --- PASS: TestConformance/WatchDeliversInitialAndLive (0.03s)
    --- PASS: TestConformance/UpdateStatusKeepsSpecAndManager (0.10s)
```

By default the harness skips the impossible case with its reason, so `go test ./...` stays green. `KUDURRU_CONFORMANCE_STRICT=1` runs it anyway, as shown above.

```
$ go test -count=1 -run TestConformance -v ./internal/store/kube/
    conformance_test.go:33: impossible against a real apiserver: re-applying an identical object is a no-op ...
    --- SKIP: TestConformance/ApplyOptimisticConcurrency (0.00s)
--- PASS: TestConformance (5.48s)
```

The first case takes about 5 s because it pays the one-off discovery warm-up of a freshly started apiserver; see Surprises.

| Case | Result | Notes |
|---|---|---|
| ApplyCreatesWithManagedBy | pass | The apiserver stamps uid and resourceVersion; CRD generation starts at 1. |
| ApplyRejectsOtherManager | pass | Pre-check on the annotation; Force takes over. |
| ApplyOptimisticConcurrency | **impossible** | See below. |
| DeleteAndNotFound | pass | |
| ListFiltersByKindNamespaceLabels | pass | `limit`/`continue` are the apiserver's own keyset tokens. The last page returns an empty continue. |
| WatchDeliversInitialAndLive | pass (after a fix) | Without the catch-up barrier this case failed: the informer had synced empty before `a` was written, so `Bookmark` arrived before `Added a`. |
| UpdateStatusKeepsSpecAndManager | pass | The status subresource ignores spec, metadata and generation. |

### Why `ApplyOptimisticConcurrency` is impossible, and the exact suite change

The case applies the identical `zone("home","home.example.")` twice and requires a new resourceVersion and `generation == 2`. Against kube-apiserver:

- An identical server-side apply is a no-op: nothing is written to etcd and the resourceVersion is unchanged. `TestApplyNoOpKeepsVersion` pins this.
- CRD `metadata.generation` only increments when non-metadata, non-status content changes. A driver cannot bump it without changing the spec. Forcing a write through an annotation would change the resourceVersion but still leave generation at 1.

Proposed change to `internal/store/conformance/conformance.go`. It keeps the intent (a write changes the resourceVersion and bumps generation; a stale resourceVersion is a conflict) and holds for every driver:

```diff
 func testOptimistic(t *testing.T, s store.Store) {
 	ctx := context.Background()
 	first, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
 	if err != nil {
 		t.Fatal(err)
 	}
-	second, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
+	changed := zone("home", "home.example.")
+	changed.Spec.DefaultTTL = 600 // a spec change: every driver writes and bumps generation
+	second, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: first.GetResourceVersion()})
 	if err != nil {
 		t.Fatal(err)
 	}
```

(`zoneName` cannot be the field that changes: the CRD makes it immutable with `self == oldSelf`.) `TestApplyOptimisticConcurrencyWithSpecChange` runs exactly this body against `memory` and `kube`, and both pass.

Optional follow-up for the suite and the reference driver: a case `ApplyNoOpKeepsGeneration` requiring that an identical re-apply succeeds and does not bump generation. The resourceVersion may stay the same or change. The memory driver would need to bump generation only on spec changes, which is Kubernetes semantics and keeps `observedGeneration` meaningful for controllers.

## Elector (D44)

`LeaseElector` wraps client-go `leaderelection.LeaderElector`, configured as follows:

- Lease lock, 15 s / 10 s / 2 s timings, `ReleaseOnCancel: false`.
- It runs in a loop owned by the caller's context. Each acquisition starts a term. When the lease is lost or `ctx` ends, the term's context is cancelled, `onStopped` runs after `onStarted` returned, and on lease loss the loop re-enters the election after `RetryPeriod`.
- `Run` returns `nil` only when `ctx` is cancelled.
- On shutdown the Lease is released only after the term has fully stopped, and only if it still names this identity. That is the "ReleaseOnCancel on SIGTERM" semantics of D44.

client-go's own `ReleaseOnCancel` is not used for two reasons: it releases while `OnStartedLeading` may still be running, and after a lost lease it can clear a successor's record when it acts on a stale observed record.

A panic in leader-only work is recovered and logged. The elector then gives the lease away and sits out one `LeaseDuration`, so the process (and DNS) survives and another replica leads.

```
$ go test -count=1 -run Elector -v ./internal/store/kube/        # 4 s / 2 s / 250 ms test timings
--- PASS: TestNewLeaseElectorValidation (0.00s)
--- PASS: TestLeaseElectorSingleLeader (2.31s)
    elector_test.go:254: partition → pod-a onStopped: 2.26s; partition → pod-b onStarted: 3.968s
--- PASS: TestLeaseElectorFailoverOnExpiry (12.73s)
    elector_test.go:308: lease deleted → pod-b onStarted: 301ms (no expiry wait)
--- PASS: TestLeaseElectorLeaseDeleted (6.27s)
--- PASS: TestLeaseElectorHijack (6.64s)
--- PASS: TestLeaseElectorPanicStepsDown (0.11s)
--- PASS: TestLeaseElectorRunWithContext (0.06s)
--- PASS: TestStoreElector (0.06s)

$ KUDURRU_MEASURE=1 go test -count=1 -run TestMeasureElectorProductionTimings -v ./internal/store/kube/
    measure_test.go:322: first acquisition: 50ms
    measure_test.go:331: production timings: partition → old leader onStopped 11.989s (RenewDeadline 10s + ≤ RetryPeriod),
        → new leader onStarted 17.708s (LeaseDuration 15s after last renew); old leader Run still running: true; max concurrent terms 1
    measure_test.go:340: graceful shutdown of the leader (lease released) → successor onStarted: 202ms
```

On graceful shutdown the successor's takeover is bounded by one jittered `RetryPeriod`. It measured 0.2 s here and 2.5 s in an earlier run.

How each scenario works:

- **Partition (`FailoverOnExpiry`).** Each candidate has its own transport that can be cut. With pod-a cut off, its renewals fail. pod-a stops leading at `RenewDeadline` while its `Run` keeps going. pod-b takes the lease once it expires. After the partition heals, pod-a stays a follower. An arena counter checks that at most one term is ever active.
- **Lease deleted.** pod-b creates a new Lease at once. pod-a's term ends after `RenewDeadline`, and its `Run` keeps going.
- **Hijack.** Another identity writes itself into the Lease. The leader ends its term on the next renewal, re-enters the election, and takes the Lease back once the intruder's lease expires.

## Measurements

Setup: 8 vCPUs, 16 GB RAM, Linux, Go 1.27.2, kube-apiserver/etcd 1.37.0 from envtest on loopback, `CGO_ENABLED=0`. Command:

```
KUDURRU_MEASURE=1 go test -count=1 -run TestMeasure -v -timeout 20m ./internal/store/kube/
```

The objects are realistic DNSRecordSets: 2 endpoints (A with 2 targets, TXT), 2 labels, 350 bytes of JSON each.

| What | Result |
|---|---|
| **List 1,000 DNSRecordSets, paginated**, 7 runs, median (min to max) | limit 100 (10 pages): **48.6 ms** (47.4 to 53.6). limit 250 (4 pages): 47.9 ms (43.6 to 57.7). limit 500 (2 pages): 43.5 ms (43.2 to 51.7). No limit: 45.0 ms (43.3 to 52.5). |
| List, label selector `team=web` (250 matches), first page of 100 | 4.9 ms median |
| Apply, SSA create, 1,000 objects, 8 workers | 810 ms, 1,235 objects/s |
| Apply, SSA update, sequential (metadata GET plus PATCH) | 3.09 ms median (2.92 to 3.48) |
| **Informer cache memory, 1,000 DNSRecordSets** | Driver with 5 synced empty informers: +0.64 MiB heap. With 1,000 DNSRecordSets: **+2.29 MiB, about 1.7 KB per object**. Sync takes 105 ms. One informer that keeps `managedFields`: +2.36 MiB for that kind alone. |
| Watch on a synced informer, including the catch-up list | 1,000 `Added` then `Bookmark` in 21 ms |
| MaterializedSnapshot (1,001 objects) | 2.5 ms median |
| local.db Write / Load (1,001 objects) | 13.7 ms / 8.4 ms median. Main file 828 KiB. WAL 4 MiB after 5 rewrites, then checkpointed. |
| `Snapshot` (`kind: List` YAML, paged) | 178 ms, 625 KiB |
| First requests on a fresh apiserver | Control plane start plus CRD install 2.8 s. First List (RESTMapper discovery) 1.0 s. First SSA of a kind 9 ms. WaitForSync for 5 informers 101 ms. |

List latency barely depends on page size (about 45 ms per 1,000 objects, dominated by decoding), so the UI can page at 100 with no penalty.

### Binary size

Build command for each variant: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`. `cmd/kudurru` does not import the driver, so the variants are throwaway copies of `cmd/kudurru` with one extra file that links the named code. The copies were not committed.

| Binary | Bytes | Δ vs `cmd/kudurru` |
|---|---:|---:|
| `cmd/kudurru` before and after this spike (unchanged) | 11,813,024 | n/a |
| + client-go and controller-runtime as the driver uses them (client, cache, typed coordination client, leaderelection), no sqlite | 31,490,208 | **+19,677,184 (+18.77 MiB)** |
| + local.db only (modernc.org/sqlite) | 17,739,936 | +5,926,912 (+5.65 MiB) |
| + full `kube` driver (it imports `localdb`, so sqlite is included) | 35,221,664 | +23,408,640 (+22.32 MiB) |
| + driver + `PersistSnapshots` + `SnapshotStore` | 35,557,536 | +23,744,512 (+22.64 MiB) |
| *Reference:* client-go REST client + informer over the kudurru scheme only | 15,982,752 | +4,169,728 (+3.98 MiB) |

The client-go/controller-runtime path costs 14.8 MiB more than the REST-only variant (31,490,208 − 15,982,752 bytes). The gap is dominated by `k8s.io/client-go/kubernetes/scheme`. Both controller-runtime's client and client-go's typed clients (and therefore `leaderelection/resourcelock`) import it, and it registers all 56 `k8s.io/api` packages: `go list -deps` finds 56 for the driver's imports and 0 for the REST-only variant. In the unstripped binary, `k8s.io/api` alone accounts for 7.2 MiB of symbol text and data. sqlite is paid once for `state.db`, `local.db` and `querylog.db` (D33's "one engine").

### Cold start from local.db (§4.4)

`TestColdStartFromLocalDB` runs on a dedicated control plane:

1. A live driver writes its materialised set through `PersistSnapshots`: an initial write after sync, then a change that reaches local.db on the next tick.
2. The driver is closed and envtest's kube-apiserver and etcd are **stopped**.
3. A new driver against the dead apiserver gets `WaitForSync == false` after `StartupTimeout` (1 s in the test, 15 s by default), and `MaterializedSnapshot` returns `ErrNotSynced`. That is the fallback signal.
4. A reader with no Kubernetes client (`localdb.OpenSnapshotStore` + `Load`) finds every object, every kind including BlockList and ClientGroup, uid, resourceVersion and managed-by intact, the per-kind resourceVersions, the high-water mark, the cluster ID (the kube-system namespace UID) and the age.

```
--- PASS: TestColdStartFromLocalDB (6.33s)
    coldstart_test.go:168: cold start: 6 objects, high-water mark 236, age 2.094s, cluster d281ab9d-054c-4263-9c5a-1a0d4871c0f9
```

## RBAC actually needed (envtest runs with `--authorization-mode=RBAC`)

`TestLeastPrivilegeRole` runs every driver operation as a user bound only to the rules below, and passes:

```
kudurru.io  dnszones,dnsrecordsets,blocklists,clientgroups,policies   get,list,watch,create,patch,delete
kudurru.io  */status                                                    update,patch
""          namespaces (resourceNames: kube-system)                     get            # ClusterID
coordination.k8s.io leases (Role in the release namespace)              get,create,update
```

`TestApplyVerbs` shows that server-side apply needs **both** `patch` and `create`: creating through SSA is authorized as `create`. `update` is not needed for spec writes.

## Surprises

1. **The worktree branch started at `Initial commit`** (LICENSE only), not at the scaffold. I fast-forwarded the branch to `79a0e37`. That is a fast-forward of my own branch, not a rebase or a branch switch. All commits sit on top of it.
2. **The generated CRDs do not install on Kubernetes 1.37.** BlockList is rejected with "estimated rule cost exceeds budget by factor of 7.1x" and ClientGroup with "1.26x". The cause: the per-item CEL rule `[has(...),...].filter(x, x).size() == 1` sits on arrays with no `maxItems`. Rewriting the rule arithmetically fixes BlockList but not ClientGroup. Adding `maxItems` fixes both (tested with 64, 256, 1000 and 4096). The test harness installs the CRDs with exactly the bounds proposed below, and the patch becomes a no-op once `api/` carries them.
3. **`BlockList` ends in "List".** A name-based registry silently dropped the kind. The cold-start test caught it through a missing high-water mark. Lists are now recognised by type.
4. **Informer lag versus the Watch contract.** An informer that synced before a write replays a state without it. The suite expects writes completed before `Watch` to be in the initial `Added` set. Fixed with a catch-up barrier: one metadata List, then wait until the cache reflects it, comparing resourceVersions as integers with `k8s.io/apimachinery/pkg/util/resourceversion` (KEP-5504). It costs about 20 ms per `Watch` call at 1,000 objects. It is best effort when the apiserver is down.
5. **SSA conflict causes read `conflict with "<manager>"`**, plus `with subresource ...` or `using <ver> at <time>` for Update managers. This is from apimachinery's `managedfields/internal/conflict.go`. The parser handles both the prefixed and the bare form.
6. **SSA create needs the `create` verb**, not just `patch`.
7. **Force via plain SSA left fields behind**: fields owned only by the previous manager (or kubectl) survived a forced apply. See Apply step 4.
8. **client-go defaults to 5 QPS / burst 10** when `rest.Config.QPS` is 0, which would make a `Restore` of 1,000 objects take minutes. The driver now defaults to 50/100.
9. **Binary size** is dominated by `client-go/kubernetes/scheme`, not by controller-runtime's own code (0.14 MiB of symbols).
10. **envtest in a host without a default route**: kube-apiserver cannot pick an advertise address. The harness passes `--advertise-address=127.0.0.1 --endpoint-reconciler-type=none`, which is harmless everywhere.
11. **First-request warm-up**: a freshly started apiserver's discovery takes 1 to 5 s for the first client. In production this falls inside `startupTimeout`.
12. controller-runtime 0.25 ships an experimental `EnableReadYourWritesConsistency` cache option. It is not used: it is scoped per representation, so typed cached reads would not wait for the driver's unstructured SSA writes, and the cache cannot serve `Continue` tokens anyway.

## Recommended changes to docs/architecture.md

I did not edit architecture.md; these are proposals.

1. **§4.3, Apply contract.** Add: "Generation increments only when the spec changes. An identical re-apply may return the stored resourceVersion and generation." This is apiserver behaviour and should hold for every driver (memory and sqlite included). Also add: "Force is a whole-object takeover: the new manager owns every field afterwards."
2. **§4.3, driver table, kube row.** State the read path: Get, List, Snapshot and Restore are consistent apiserver reads/writes; Watch and the materialised snapshot are served from informers; Watch replays state as `Added` regardless of `WatchOptions.ResourceVersion` (Added means upsert).
3. **§4.3 Elector interface.** Give `onStarted` the term context: `Run(ctx context.Context, onStarted func(termCtx context.Context), onStopped func()) error`. Today the child context exists only in `kube.LeaseElector.RunWithContext`. The memory and sqlite always-leader electors would pass `ctx`.
4. **§5.3 / D44.** D44 says `internal/cluster` drives client-go's elector. The §4.3 table (and this spike) put the loop in the kube driver behind `Store.Elector`. Proposed wording: "internal/cluster runs leader-only runnables through `Store.Elector`; the kube driver's elector owns the client-go loop."
5. **§5.3 Lease names.** `Elector(name)` maps to `kudurru-<name>`, so `controller` becomes `kudurru-controller` as documented, but integrations become `kudurru-integration-<ns>-<name>`, while §5.3 says `integration-<ns>-<name>`. Pick one. I recommend the prefixed form, which is greppable and avoids collisions in shared namespaces.
6. **§5.3 ClusterRole.** Add `create,patch,delete` on `kudurru.io/*`. The SA performs API and UI writes after the SAR check (v0.1), and SSA needs `create` as well as `patch`. Add `patch` next to `update` on `*/status`. `get` on `namespaces/kube-system` is enough for the cluster ID. Lease verbs are confirmed.
7. **§4.4.** Replace "the resourceVersion high-water mark" with "per-kind list resourceVersions (informers are per resource; resourceVersions are comparable per resource, KEP-5504) plus their maximum". Also name one owner for local.db migrations (a future `internal/localdb`) before audit and zone data move in. This spike uses the shared `meta.schema_version` and `kube_snapshot*` table names.
8. **§2 / §14 (risk table).** Record the binary-size cost: about 33.9 MiB stripped with the kube driver. Of that, 14.8 MiB is the gap between the controller-runtime/typed-client path and a REST-only client, dominated by `client-go/kubernetes/scheme`. The lean alternative (REST client + informers over the kudurru scheme + a Lease elector on that client) measured +4 MiB, but it means giving up controller-runtime and client-go's `leaderelection`, which D44 relies on. I do not recommend it for v0.1. It is an option if image size becomes a requirement. v0.2 external zones need core, networking and gateway types anyway.

## Changes outside this scope (exact proposals)

**`api/v1alpha1` (CRD install bug)**, followed by `make generate`:

```diff
 // BlockListSpec
 	// +kubebuilder:validation:MinItems=1
+	// +kubebuilder:validation:MaxItems=256
 	Sources []ListSource `json:"sources"`
 // ClientGroupSpec
+	// +kubebuilder:validation:MaxItems=1000
 	Clients []ClientMatcher `json:"clients,omitempty"`
```

**CI (`.github/workflows/ci.yml`)**: add `make envtest` before `make test`, so the kube driver tests run in CI instead of skipping. The binaries are about 120 MB, so caching `bin/k8s` is worthwhile.

**`cmd/kudurru` wiring (later milestone)**:

- `ctrllog.SetLogger(logr.FromSlogHandler(...))` and `klog.SetSlogLogger(...)`, so controller-runtime and client-go log through slog. Without the first, controller-runtime prints a "log.SetLogger(...) was never called" warning.
- Gather `sigs.k8s.io/controller-runtime/pkg/metrics.Registry` into `/metrics`.
- The cold-start path: `WaitForSync` → `localdb.Load` → `Degraded{ServingFromSnapshot}`.

## Follow-ups

- Apply the conformance and `api/` changes above (their owners), then drop `knownImpossible` and `celBudgetPatches` from the harness.
- Internal kinds (Secrets labelled `kudurru.io/kind=<internal>`, §4.5) in the kube driver, and `SnapshotOptions.Safe` stripping for them.
- `WatchOptions.ResourceVersion`: decide between "replay as Added" (current behaviour, documented) and a true resume. Informers cannot resume from an arbitrary RV.
- zonecache: use `PersistSnapshots` (30 s) and the `Fresh(maxAge)` gate; export `kudurru_store_snapshot_serving` and `kudurru_store_snapshot_age_seconds` from `Snapshot.Age`.
- Context on `store.Elector.Run` (recommendation 3), then have `internal/cluster` use it.

## Reproduce

```
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
make envtest                                   # bin/k8s/1.37.0-linux-amd64
go test -race -count=1 ./internal/store/...    # conformance, elector, cold start, RBAC
KUDURRU_CONFORMANCE_STRICT=1 go test -count=1 -run TestConformance -v ./internal/store/kube/
KUDURRU_MEASURE=1 go test -count=1 -run TestMeasure -v -timeout 20m ./internal/store/kube/
KUDURRU_TEST_LOGS=1 ...                        # show driver, controller-runtime and klog output
```

Hygiene at the final commit:

- `gofmt -s -l ./internal/store` is empty.
- `go vet ./...` is clean.
- `golangci-lint run ./...` reports 0 issues.
- `go test -race -count=1 ./...` is green, and so is `-count=4` on `internal/store/kube`.
- The licences of every module linked by `internal/store/kube` are Apache-2.0, BSD, MIT or ISC. No GPL, EUPL or MPL.
