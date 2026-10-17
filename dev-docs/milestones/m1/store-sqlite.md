# M1 work package: `store-sqlite`

The `sqlite` Store driver: `internal/store/sqlite`, the `state.db` behind the
"Single Docker" and "Kubernetes standalone" shapes (architecture §4.3, §4.4,
§4.5, §6.3, §11.4; decisions D3, D9-D12, D30, D70).

## What was built

- **state.db on modernc.org/sqlite** (cgo-free). Every connection sets WAL,
  `synchronous=NORMAL`, `busy_timeout` (default 5 s) and `foreign_keys=1`.
  There is one writer connection (`_txlock=immediate`, so every write takes
  the lock up front) and a `query_only` reader pool (default 4). The file is
  created `0600` in a `0700` directory. An `flock(2)` on `state.db.lock`
  stops a second process from opening the same database (`ErrLocked`).
  `Close` checkpoints the WAL (`TRUNCATE`).
- **Schema (migration 1)**, all `STRICT`:
  - `objects(kind, namespace, name, uid, generation, resource_version,
    managed_by, created_at, updated_at, spec_hash, labels, deleting, data)`,
    keyed `(kind, namespace, name)` and `WITHOUT ROWID`. `data` holds the
    whole object as JSON. `labels` is a JSON copy that List's selector reads
    through `json_each`.
  - `changes(seq, kind, namespace, name, type, resource_version, tombstone)`:
    the change feed. `seq` is the write's resourceVersion. Deletes keep the
    final object as a tombstone. The feed is compacted to `FeedRetention`
    rows (default 100,000), checked every 1,024 writes.
  - `meta`: `schema_version` (= `store.SchemaVersion`), `cluster_id` (a
    UUID generated once), `cluster_epoch` (1), the `resource_version`
    counter, `feed_compacted_through` and `created_at`.
  - `audit(id, ts, actor, manager, action, kind, namespace, name,
    resource_version, result, reason, before, after, prev_hash, hash)`.
    Every mutation writes its audit row in the same transaction (D30). Rows
    are hash-chained (§11.4). `before`/`after` are capped at 4 KiB; a larger
    document is replaced by `{"truncated":true,"bytes":N,"sha256":…,"prefix":…}`.
- **resourceVersion** is one database-wide integer counter, stored in
  `meta`, bumped inside the write transaction and rendered as a decimal
  string. It only increases and survives restarts.
- **Schema versioning (D11).** `migrate` runs on every open, inside one
  transaction. A newer on-disk version returns `ErrSchemaTooNew`, and the
  message names both versions and the fix. The database is left untouched
  and the lock is released. An older version runs each missing step in
  order. A failed step rolls back everything. A gap in the steps is an
  error. `TestMigrationsCoverSchemaVersion` checks that the last migration
  equals `store.SchemaVersion`.
- **The full `store.Store`:**
  - `Get`.
  - `List`: keyset pagination ordered by `(namespace, name)`. `Continue`
    is an opaque base64url token naming the kind and the last key. Equality
    label selectors are supported.
  - `Apply` follows the Apply contract:
    - It runs `validator.Validate` before the write; the validator defaults
      the object.
    - `managedBy` is the single writer (`*store.ErrManagedBy`). `Force`
      takes over the whole object and is audited with the previous owner.
    - `ExpectedResourceVersion` gives optimistic concurrency
      (`store.ErrConflict`, also for a precondition on a missing object).
    - Status is carried over from the stored row.
    - Generation increments only when the spec hash changes.
    - An identical re-apply writes nothing and returns the stored object.
    - A caller-stamped uid and creationTimestamp are kept (D10) and are
      generated only when absent.
    - The Apply that clears the last finalizer of a terminating object
      removes the object.
  - `Delete`:
    - `managedBy` is honoured when a Manager is given; an empty Manager is
      unchecked, matching the other drivers.
    - An object with finalizers gets a deletionTimestamp (a Modified event)
      instead of being removed.
    - Deleting an object that is already terminating is a no-op.
  - `UpdateStatus`: runs `validator.ValidateStatus`, leaves spec and
    metadata untouched and never moves generation. A non-empty
    `resourceVersion` on the argument acts as a precondition. An unchanged
    status writes nothing.
  - `Watch`:
    - It subscribes to an in-process broadcaster first, then reads.
    - Without a resourceVersion, the watcher gets the current objects as
      `Added`, then a `Bookmark` carrying the list's resourceVersion, then
      live events in resourceVersion order.
    - With a resourceVersion, it replays the change feed after that version
      (coalesced to one event per object), then sends the Bookmark. This
      also works across restarts.
    - Below the compaction horizon, it relists.
    - A slow consumer never blocks writers. Its watcher drops off the
      broadcaster and catches up from the feed. After a compaction it
      relists and emits `Deleted` for objects that disappeared.
    - Asking for a resourceVersion the store never reached returns
      `ErrResourceVersionTooNew`.
  - `Elector`: always the leader. It records the election name and reports
    `Options.Identity`.
  - `Snapshot`: `kind: List` YAML from one read transaction, sorted by
    key. `Safe` drops `Options.InternalKinds`.
  - `Restore`: decodes YAML/JSON (lists or streams) through the
    `api/v1alpha1` scheme and Applies each object (see Surprises).
  - `Info`, `Close`: idempotent; `Close` stops every watcher.
- **Audit API** for later use by `GET /audit`, doctor and retention:
  - `ReadAudit(ctx, AuditQuery{Kind, Namespace, Name, AfterID, Since,
    Limit})`.
  - `VerifyAudit(ctx)`: walks the hash chain; a pruned head is accepted.
  - `RecordAudit(ctx, AuditEntry)`: rows not tied to a store write, such as
    logins, denials and runtime actions.
  - `PruneAudit(ctx, before)`: 400-day retention.
  - `WithActor(ctx, actor)` / `ActorFrom(ctx)` name the principal. An empty
    actor is recorded as the write's Manager.

The conformance suite passes unchanged (`conformance.Run` with a temp-dir
store per case). Driver tests:

- persistence across reopen (identity, counter, status)
- WAL layout, file modes and pragmas, and a checkpoint on close
- second-process lockout
- schema refusal
- migration framework: step-by-step, rollback and gap cases
- caller-stamped identity and validation rejections
- returned objects equal stored objects, timestamps included
- finalizers
- audit: rows per mutation, atomicity, the 4 KiB cap, RecordAudit, prune
- Watch: resume from a resourceVersion, resume across reopen, namespace
  filter, slow consumer, compaction relist, stop and close
- Snapshot/Restore round trip and Safe
- 10,000-object timing
- concurrent writers: no lost updates, unique resourceVersions, exactly one
  strict winner

## Wiring (`cmd/kudurru serve`, `store.driver: sqlite`)

```go
import (
    "path/filepath"

    "github.com/robertherbaugh/kudurru/internal/store"
    "github.com/robertherbaugh/kudurru/internal/store/sqlite"
    "github.com/robertherbaugh/kudurru/internal/store/validator"
)

v, err := validator.New()                 // build once per process; share with the API layer
st, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{
    Validator: v,                         // nil: Open calls validator.New() itself
    Identity:  cfg.Node.Name,             // Info().Leader / Elector.Leader(); default hostname
    Logger:    logger.With("component", "store"),
    // Optional: Scheme (default: fresh scheme + v1alpha1), BusyTimeout (5s),
    // FeedRetention (100_000), ReadConns (4), InternalKinds (for Snapshot Safe),
    // ActorFromContext (default sqlite.ActorFrom), Now (tests).
})
switch {
case errors.Is(err, sqlite.ErrSchemaTooNew): // refuse to start; doctor explains (D11)
case errors.Is(err, sqlite.ErrLocked):       // another kudurru holds state.db
}
defer st.Close()
var _ store.Store = st                    // *sqlite.Store
```

- **Signature:** `func Open(path string, opts sqlite.Options) (*sqlite.Store, error)`.
- **Readiness:** a successful `Open` means the store is ready, since there
  is nothing to sync.
- **Audit actor:** the API middleware should call
  `ctx = sqlite.WithActor(ctx, "user:"+id)` (or set
  `Options.ActorFromContext` to read the auth package's principal), so audit
  rows name the human and not only the manager.
- **Audit retention:** a daily leader loop
  `st.PruneAudit(ctx, time.Now().AddDate(0,0,-400))`.
- **Other exported names:** `sqlite.DriverName`, `sqlite.FileName`,
  `(*Store).Path()`, `(*Store).ResourceVersion()`,
  `(*Elector).Name()`, `AuditEntry`, `AuditQuery`, `AuditDiffCap`,
  `AuditSuccess|AuditDenied|AuditError`, `ErrResourceVersionTooNew`.

## Measurements

Go 1.27.2, linux/amd64, a shared 8-CPU sandbox, no race detector
(`go test -run TestTenThousandObjects -v`, `go test -bench . -benchtime=2000x`).
DNSRecordSet objects with one endpoint and one label.

| What | Value |
|---|---|
| Apply create, p50 / p99 (10k sequential, validation included) | 0.34 ms / 3.9-4.3 ms |
| Apply update with a spec change, p50 / p99 | 0.45 ms / 1.3-2.1 ms |
| Apply identical (no-op), mean | 0.20 ms |
| BenchmarkApplyCreate / Update | 451 µs / 491 µs per op; 83 KB / 134 KB, 995 / 1,506 allocs |
| Get, p50 / p99 (10k rows) | 15 µs / 31-38 µs (benchmark: 11.4 µs, 34 allocs) |
| List all 10,000 | 77-80 ms |
| List 10,000 in 20 pages of 500 | 73-82 ms (keyset; each page costs about the same) |
| List with namespace and label selector (500 hits of 10k) | 4.3-4.8 ms |
| Seed 10,000 objects through Apply | 4.2-4.4 s |
| state.db after 11,000 writes (10k objects), checkpointed | 16.9 MB, 1,543 B per write overall |
| — objects table (incl. PK index) | 7.2 MB, about 720 B per object |
| — audit table (11k rows, `after` JSON included) | 8.9 MB, about 810 B per row |
| — change feed (11k rows) | 0.84 MB, about 76 B per row |
| Open with 10k objects, shared validator | 1.1-1.6 ms |
| Open with 10k objects, `Validator: nil` (builds `validator.New()`) | about 10 ms |
| Under `-race`, the whole package | about 70 s (the 10k test about 40 s) |

An Apply costs about half validation and half write. The no-op path (read,
then CEL plus defaulting through the JSON round trip, with no write) is
about 0.2 ms. The write transaction (five statements plus a WAL commit with
`synchronous=NORMAL`) adds about 0.25 ms. Writes are serialised by design,
but validation runs outside the write lock, so sequential throughput is
about 2-3k writes/s, far more than a config store needs.

## Surprises and decisions

- **The previous attempt's branch had been reset to `origin/main`.** The
  work was still in the reflog (`524d16a` driver, `e0005c0` WIP). I
  fast-forwarded to it and squashed the WIP into proper commits.
- **Restore keeps the recorded owner.** The brief says "Apply with Manager
  `bootstrap` for each document". The driver applies each object with its
  recorded `kudurru.io/managed-by` owner and falls back to `bootstrap` only
  when there is none. This is exactly what the kube driver's `Restore` does.
  It keeps file- and integration-managed objects owned by their writer after
  a restore; otherwise the next file sync would get `ErrManagedBy`. An
  object with no owner gets `bootstrap`, as the brief asks.
- **UpdateStatus does not clear finalizers.** The brief says "remove on the
  next Apply/UpdateStatus that clears them". UpdateStatus behaves like the
  status subresource: `validator.ValidateStatus` takes metadata from the
  stored object, and the kube driver cannot change finalizers through
  `/status` either. So only an Apply can clear finalizers, and that Apply
  removes the object (audited as `<kind>.finalize`). Letting UpdateStatus
  touch metadata would break parity with the kube driver and D70.
- **Timestamps the driver stamps are normalised** to whole seconds in the
  local zone (`storedTime`), which is exactly how `metav1.Time` decodes.
  Without this, the object Apply returns, or the Modified event that Delete
  emits, differed from what `Get` read back by nanoseconds and by
  `time.Location`.
- **`validator.New()` is cheap** (about 10 ms here), so `Validator: nil` is
  fine for tools. Sharing one validator matters only in tests that open
  hundreds of stores.
- **Plan outside the lock, commit under it.** Reading the current row and
  validating happen outside the write mutex. The commit re-checks the row's
  resourceVersion inside the transaction. A race retries up to three times
  and then runs fully locked. With a precondition, a race is `ErrConflict`.
  This keeps the slow part (CEL) parallel while writes stay serialised.
- **No NOTICE entry is needed.** The design is the driver's own; nothing was
  ported.

## Follow-ups

- **Architecture text (§4.3/§6.3).** Suggest stating that the sqlite driver's
  Restore preserves the recorded `managedBy` (as kube does), and that
  finalizers are cleared by Apply only (status writes never change
  metadata).
- **Internal kinds** (API token hashes, local users, TSIG, sessions in v0.2)
  are not types in `api/v1alpha1` yet. When they land, pass them as
  `Options.InternalKinds` so `Snapshot{Safe: true}` strips them, or move the
  list to `store`.
- **`RecordAudit` covers only the storage half of §11.4.** The richer
  `AuditEvent` fields (`node`, actor kind and groups, `authz`, request id,
  ip, ua, trace_id) and the stdout JSON line belong to the audit package.
  The table has `reason`; add columns via migration 2 when that package
  defines its schema. Before/after documents are not yet redacted for
  secrets, because no kind carries secrets in v1alpha1.
- **`cluster_epoch` / `primary_id`** for fenced promotion (v0.2 follower) are
  ready in `meta`; promotion needs a setter.
- **List paging is not a snapshot.** Pages read the latest data, as
  documented. The kube driver's continue tokens are consistent snapshots;
  nothing in M1 depends on the difference.
- **Metrics.** `kudurru_store_*` (write latency, feed lag, watchers) are not
  wired yet; the natural hooks are `commitLocked` and the broadcaster.
