# M1 work package: file-managed objects and first-boot bootstrap

Key `objects`. Covers docs/architecture.md §6.2 (objects: / objects.d), §6.4
(first boot), §4.3 (Apply contract, managedBy), §4.4 (ha-static read-only
flag) and D9.

## What was built

| Package | Contents |
|---|---|
| `internal/objects` | `Loader` (load, reconcile, watch), `Diagnosis`/`Report`, `Status`/`Ready` for /healthz, `kudurru_objects_*` metrics, `ApplyFile`/`ApplyBytes` |
| `internal/bootstrap` | `Seed` (first-boot defaults), `Objects` (the seed set), `StarterBlockListFunc` seam |
| `internal/config` | two keys: `objects` (list) and `objectsDir` (string, default `/etc/kudurru/objects.d`); both `hostOnly`, `restartRequired`, scope node. `TestSchemaMatchesConfig` stays green; `TestLoadObjects` added |

No changes to cmd/, the store, the validator or any other agent's package.
`go.mod` gains `github.com/kylelemons/godebug` as an indirect dependency
(pulled in by `prometheus/client_golang/prometheus/testutil` in tests).

### Loader semantics

* **Sources, in apply order:** the inline `objects:` list (source name
  `kudurru.yaml`, override with `Options.InlineSource`), then every `*.yaml`
  directly in `objectsDir`, sorted by name. Sub-directories, dot files and
  other extensions (including `.yml`) are ignored; symlinks are followed (so
  ConfigMap mounts with `..data` work). A missing directory counts as empty.
* **Manager:** `file:<source>`, where a file's source is
  `<base(objectsDir)>/<file>`, e.g. `file:objects.d/zones.yaml`. That is what
  the UI shows in "managed by ...".
* **Decoding:** multi-document YAML. Duplicate keys are errors (strict YAML).
  Documents of kind `List` are flattened, so `Store.Snapshot` output and
  `kubectl get -o yaml` load as they are. Server-populated metadata (uid,
  resourceVersion, generation, creationTimestamp, managedFields), `status` and
  any `kudurru.io/managed-by` annotation in the file are stripped. Namespaced
  objects without a namespace go to `Options.DefaultNamespace` (`default`).
* **Validation:** `validator.ValidateUnstructured` (strict unknown fields, all
  schema defaults, CEL), with the stored object as `old` so transition rules
  such as "zoneName is immutable" apply. Then
  `FromUnstructuredWithValidation(strict)` turns the result into the
  `api/v1alpha1` type, and `Validate` runs on the typed object (see
  Surprises for why both are needed).
* **Unchanged detection:** when the stored object already matches (everything
  except metadata/status, plus labels and annotations), nothing is written.
  The 60 s poll therefore costs no writes and no watch events. This matters
  because the memory driver bumps resourceVersion on every Apply. The loader
  also remembers each document's hash together with the stored
  resourceVersion it last matched. When both are unchanged, the poll skips
  validation and comparison entirely. Any store write (drift, a status
  update) moves the resourceVersion and takes the full path.
* **Ownership ledger:** the store is the ledger, so there is no side file. An
  object whose `managedBy` starts with `file:` and that no source declares any
  more gets deleted, whichever file it came from. That covers deleting a
  document, deleting a file, and changing `objectsDir`.
* **Ownership decisions for an existing object:**
  * same manager: update, or leave it alone if unchanged.
  * no manager: adopt it (Apply contract rule 2) and record `Adopted` (info).
  * `bootstrap`: `Force` takeover, recorded as `TakenOver` (info). The §6.2
    example declares `Policy/default` inline; without the takeover it would
    conflict with the first-boot seed.
  * another file that no longer declares it: `Force` takeover, recorded as
    `Moved` (info). Moving a document between files keeps its uid, so there
    is no delete/create flap.
  * `api`, `system`, `integration:*`: the object is left alone and a
    `ManagedByOther` error is recorded, with the owner and a hint.
* **Never abort a load.** Every problem becomes a `Diagnosis{Severity, Reason,
  Source, Document, Object, Owner, Fields, Message, Hint}` and the rest is
  applied. The reasons are: `ReadFailed`, `InvalidYAML`, `NotAnObject`,
  `UnknownKind`, `Invalid`, `Duplicate`, `ManagedByOther`, `OwnerUnreadable`,
  `ApplyFailed`, `DeleteFailed`, `ListFailed`, and the info reasons
  `Adopted`, `TakenOver`, `Moved`. Diagnoses are sorted by source, then by
  document.
* **Last-good protection:** if a source cannot be read, or has any document
  that cannot be identified (YAML error, not a mapping, missing
  apiVersion/kind/name, unknown kind), nothing it owns is deleted or taken
  over during that pass. An identified but invalid document keeps the stored
  object. An unreadable directory (anything other than not-exist) protects
  everything under `file:<dir>/`.
* **Duplicates:** the first declaration wins (inline first, then files in
  name order). Later declarations get `Duplicate` with "already declared in
  <source>". This applies within one file too.
* **Watch:** fsnotify on the directory. Any event re-arms a 500 ms debounce
  timer. A poll every 60 s also runs a full load, which repairs store drift
  (for example a force-deleted file object) and re-attaches the watch when
  the directory was missing or got replaced. `Trigger()` asks for a reload
  without blocking. Loads are serialised.
* **ha-static flag:** `Options.Static` controls `APIReadOnly()` and
  `CheckAPIWrite()`, which returns `ErrStaticReadOnly` for all kudurru.io
  objects. This is the flag only; the API has to call it.

### Bootstrap semantics

* The seed set, in creation order:
  1. Optional starter `BlockList` from `Options.StarterBlockList`.
  2. `ClientGroup/<ns>/default` with `cidr 0.0.0.0/0` and `cidr ::/0`.
  3. `Policy/<ns>/default` with `clientGroupRefs: [default]`, plus
     `blockListRefs: [<starter>]` when there is a starter list.
  4. `DNSZone/root` with `zoneName: "."`, `type: forward` and
     `forward.upstreamGroup: default`.

  Every seed is written with Manager `bootstrap`.
* **First boot means none of the seeded kinds has any object.** In that case
  every seed that is absent gets created. If anything exists (files, API, a
  restore, an earlier boot), `Seed` does nothing at all. A default the
  operator deleted stays deleted, and an edited default is never overwritten.
* With `Options.Validator` set, the whole set is validated (and defaulted)
  before the first write, so an invalid seed cannot leave a partial first
  boot behind.
* Concurrent replicas are safe: they make identical writes under the same
  manager, and an `ErrManagedBy` race counts as "present".

## Public API

```go
// internal/objects
func New(opts objects.Options) (*objects.Loader, error)
type Options struct {
    Store            store.Store            // required
    Validator        *validator.Validator   // default: shared validator.New()
    Inline           []map[string]any       // cfg.Objects
    InlineSource     string                 // default "kudurru.yaml"
    Dir              string                 // cfg.ObjectsDir; "" disables
    DefaultNamespace string                 // default "default"
    Debounce         time.Duration          // default 500ms
    PollInterval     time.Duration          // default 60s; <0 disables
    Registerer       prometheus.Registerer  // nil: metrics not registered
    Logger           *slog.Logger
    Static           bool                   // cluster.role static (ha-static)
}
func (l *Loader) Load(ctx) objects.Report        // one synchronous pass
func (l *Loader) Watch(ctx) error                // fsnotify + poll, no initial load
func (l *Loader) Run(ctx) error                  // Load + Watch
func (l *Loader) Trigger()                       // reload soon, never blocks
func (l *Loader) Status() objects.Status         // JSON-ready for /healthz and the UI
func (l *Loader) Diagnoses() []objects.Diagnosis
func (l *Loader) Ready() (bool, []string)        // health.ReadyFunc
func (l *Loader) APIReadOnly() bool
func (l *Loader) CheckAPIWrite() error           // ErrStaticReadOnly in ha-static
func objects.Manager(source string) string       // "file:" + source
func objects.ApplyFile(ctx, st store.Store, path string, opts ...objects.ApplyOption) (objects.Report, error)
func objects.ApplyBytes(ctx, st, name string, data []byte, opts ...objects.ApplyOption) (objects.Report, error)
// options: WithManager (default "api"), WithValidator, WithDefaultNamespace, WithLogger

// internal/bootstrap
func bootstrap.Seed(ctx, st store.Store, opts bootstrap.Options) (bootstrap.Result, error)
func bootstrap.Objects(ctx, opts bootstrap.Options) ([]store.Object, error)
type Options struct {
    Namespace        string                 // default "default"
    UpstreamGroup    string                 // default "default"
    Validator        *validator.Validator   // optional, recommended
    StarterBlockList bootstrap.StarterBlockListFunc
    Logger           *slog.Logger
}
type StarterBlockListFunc func(ctx context.Context) (*v1alpha1.BlockList, error)
type Result struct{ FirstBoot bool; Created, Present []store.Key }
```

Metrics:

* `kudurru_objects_files` (gauge): sources read by the last load.
* `kudurru_objects_applied_total{result}`: `result` is one of created,
  updated, unchanged, deleted, invalid, conflict, error.
* `kudurru_objects_diagnoses{severity}` (gauge).
* `kudurru_objects_last_load_timestamp_seconds`.

## Wiring in `kudurru serve`

The order is: open the store, then bootstrap, then the objects loader, then
zonecache.

```go
v, err := validator.New()                              // once per process; share with the store driver
st := /* sqlite.Open(...) or kube.New(...) + WaitForSync */

if _, err := bootstrap.Seed(ctx, st, bootstrap.Options{
    Validator: v, Logger: log,
    Namespace: /* "default" in Docker; the release namespace in Kubernetes */,
    // StarterBlockList: starterlist.Load, // when the embedded list lands
}); err != nil {
    return err // a store failure at start is fatal
}

inlineSource := objects.DefaultInlineSource
if cfg.Source != "" {
    inlineSource = filepath.Base(cfg.Source)
}
loader, err := objects.New(objects.Options{
    Store: st, Validator: v, Logger: log, Registerer: reg,
    Inline: cfg.Objects, InlineSource: inlineSource, Dir: cfg.ObjectsDir,
    Static: false, // cluster.role == "static" once that key exists
})
if err != nil {
    return err
}
loader.Load(ctx)                 // synchronous: objects are in the store before zonecache's first snapshot
checker.Register("objects", loader.Ready)
go func() { _ = loader.Watch(ctx) }()
// zonecache.Start(ctx, st, ...)
// API: call loader.CheckAPIWrite() before every kudurru.io object write and
// expose loader.Status() (for example under /api/v1/node/objects).
```

In multi-replica Kubernetes, run `bootstrap.Seed` and the loader's
Load/Watch under `st.Elector("objects")` (D78), so replicas holding different
ConfigMap revisions during a rollout cannot flap objects. In the single
Docker and ha-static shapes, every node runs both directly against its own
store. A SIGHUP handler can call `loader.Trigger()`.

## Measurements

* `BenchmarkLoadUnchanged` measures a steady-state poll of 500 objects in 10
  files on the memory driver:

  | Version | Time/op | Memory/op | Allocs/op |
  |---|---|---|---|
  | Final, with the agreement cache | **78 ms** | **60 MB** | **453k** |
  | Typed re-validation, no cache | 240 ms | 180 MB | 1.6M |
  | First version, without typed re-validation | 160 ms | 110 MB | 876k |

  The final cost is about 160 µs per object. Most of the rest is the memory
  driver's YAML deep copy on `Get` and `List`. At the default 60 s poll this
  is well under 0.2 % of one core.
* Test wall time with `-race`: internal/objects 2.7 s (the watch tests
  account for most of it, debounce shortened to 50 ms), internal/bootstrap
  1.1 s. The watch tests passed 5 runs in a row under `-race` on the shared
  machine.

## Surprises

* `ValidateUnstructured` alone does not produce what the drivers store.
  Structural defaulting never creates a missing parent object, so a zone
  document without `spec.policy` gets no `queryAccess: allow` and so on. The
  typed encoding of the same object, however, always carries `policy: {}`
  (it is a non-pointer struct), and the apiserver (or any driver that
  validates the typed write) then defaults it. Without a second, typed
  `Validate` the stored object would never equal the loader's desired
  object, and every poll would rewrite it in Kubernetes. The loader now runs
  both. The same gap affects the API, which should validate unstructured
  input and then the typed object, or the CRD should give `policy` a
  `default: {}`. Recommended for the API and validator owners.
* The memory driver bumps resourceVersion and emits MODIFIED on every Apply,
  even an identical one. The loader's unchanged detection avoids the churn,
  and drivers that implement D70's "identical re-apply is a no-op" pay
  nothing extra.
* The memory driver does not validate. That is why the loader always
  validates itself and does not rely on the driver.
* The sandbox runs with `CAP_DAC_OVERRIDE`, so a `chmod 000` directory is
  still readable. The unreadable-directory test replaces the directory with a
  regular file (ENOTDIR) instead.
* `prometheus/testutil` adds an indirect `go.mod` entry
  (`kylelemons/godebug`).

## Recommended architecture.md changes (not edited)

* §6.2: state the source naming (`file:kudurru.yaml` for inline objects,
  `file:<base(objectsDir)>/<file>` for files), that only `*.yaml` directly in
  the directory counts, that `kind: List` is accepted, and that a source which
  does not parse keeps the objects it owns (last good).
* §6.2/§4.3: file declarations take over `bootstrap`-managed objects and
  objects of other files that dropped them (`Force`), but never `api`,
  `system` or `integration:*` objects; those produce a `ManagedByOther`
  diagnosis.
* §6.4: "first boot" means none of DNSZone/ClientGroup/Policy (and BlockList
  when a starter list is configured) has any object. Defaults the operator
  deleted are not recreated.
* §11.1: add the four `kudurru_objects_*` metrics.

## Follow-ups

* `cluster.role` settings key (static | ...) to drive `Options.Static`, plus
  the API middleware that calls `CheckAPIWrite` (§4.4).
* Policy priority of the bootstrap `Policy/default`. It is 0, the same as an
  unprioritised user policy. The policy engine should treat it as the
  fallback (or the seed should get a low priority) once priority semantics
  are implemented.
* Reloading the inline `objects:` without a restart (when config reload
  lands, call a `SetInline` and `Trigger`; the key is `restartRequired`
  today).
* The M3 CLI `kudurru apply -f` should go through the API. `ApplyFile` is the
  in-process stand-in and uses manager `api`.
* The API should map `Diagnosis`/`ErrManagedBy` to `409 managed_by` with the
  owner hint, reusing `conflictHint` wording.
