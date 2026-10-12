# Spike (g): apiextensions + cel-go cost, and the driver-independent store validator

| | |
|---|---|
| Key | `store-validator` (architecture §14 spike list item (g); risk #10; decision D34) |
| Branch / worktree | `worktree-wf_cd87f6f4-670-4` in `.claude/worktrees/wf_cd87f6f4-670-4` |
| Code | `internal/store/validator` (commits `326d9ae`, `8cfd8d6`), `Makefile` (`validator-crds`), `go.mod`/`go.sum`, `NOTICE` |
| Date | 2026-10-11 |
| Verdict | **PASS.** Linking the validator adds **12.90 MB** to the release binary (gate: < 25 MB). Inside the single binary that will also carry the kube driver (controller-runtime), it adds **6.67 MB**. The risk #10 fallback is not needed. I built a prototype of it anyway to size it: +4.12 MB on the scaffold and +3.89 MB in the single binary, so it saves only 2.78 MB there, for much lower fidelity (see "Fallback prototype"). |

## TL;DR

* `internal/store/validator` compiles the five embedded CRDs with `k8s.io/apiextensions-apiserver` v0.37.1, the way the apiserver serves a CRD. It then runs the apiserver's custom-resource admission sequence on every write:
  1. Decode: prune, drop non-nullable nulls, coerce metadata, apply defaults.
  2. Prepare: status-subresource semantics; identity fields come from the old object.
  3. Validate: ObjectMeta, then the OpenAPI schema with ratcheting, then embedded resources, then list-type map/set, then x-kubernetes-validations CEL including `oldSelf` transition rules.

  Both drivers therefore reject the same inputs, because they run the same library code in the same order.
* Rejections are a field-path-aware `*validator.Error`. Its message is byte-identical to the apiserver's 422 (it is built with `apierrors.NewInvalid`). It maps to the API's `400 {error:{key:"invalid",message,hint}}`. `FromStatusError` turns an apiserver 422 into the same type, for the kube driver.
* Measured cost:
  * Binary: 11.81 → 24.71 MB.
  * Linked modules: 25 → 69.
  * Cold build: about 2× (median 10.7 → 20.4 s wall, 47 → 96 CPU-s).
  * Warm relink: 0.34 → 0.74 s.
  * Package init: about 1.6 → 7 ms.
  * `validator.New()`: 12–14 ms.
  * First `Validate`: 0.45–1.1 ms. Steady state: ≈ 0.22 ms per update with a CEL transition rule.
  * RSS at start: `version` 10.0 → 19–20.5 MB, `serve` 12.2 → 27.3 MB.
* **Biggest surprise:** the BlockList and ClientGroup CRDs generated today **cannot be installed on a real apiserver**. Their CEL one-of rules sit on items of lists that have no `maxItems`, so the static cost estimate exceeds the budget by 7.1× and 11.8×. A test now pins this, and a second test proves the fix (`maxItems` ≤ 65536). The fix belongs in `api/v1alpha1`, which is outside this spike's scope.
* Second surprise: some schema defaults can never apply to typed Go objects. `metav1.Duration` fields with `omitempty` always serialise as `"0s"`, and `ForwardSpec.UpstreamGroup` has no `omitempty`. This is equally true against a real apiserver. I verified the fix (`omitzero`).

## What was built

```
internal/store/validator/
  validator.go   Validator, New, NewFromCRDs, Validate, ValidateStatus, ValidateUnstructured
  schema.go      CRD -> structural schema, OpenAPI validators (main + status), compiled CEL
  errors.go      Error/Cause, Key/HTTPStatus/Message/Hint/Fields, Status(), FromStatusError, IsInvalid
  crds.go        //go:embed crds/*.yaml  (copies of config/crd/bases, kept in sync by `make manifests`)
  crds/          the five CRDs
  *_test.go      table, transition, ratcheting, defaults, status, unstructured, golden, CRD-install, drift, concurrency
  testdata/errors.golden
```

API (all methods are safe for concurrent use; build one instance per process):

```go
v, err := validator.New()                                  // embedded CRDs; ~13 ms
err = v.Validate(ctx, obj, old)                            // store.Store.Apply: old == nil on create
err = v.ValidateStatus(ctx, obj, old)                      // store.Store.UpdateStatus
err = v.ValidateUnstructured(ctx, u, oldU)                 // API / `kudurru apply -f` / objects.d, before typing
if ve, ok := validator.AsError(err); ok { ve.HTTPStatus(); ve.Key(); ve.Message(); ve.Hint(); ve.Causes }
ve, ok := validator.FromStatusError(apiserverErr)          // kube driver: 422 -> same *Error
v.Kinds(); v.GroupVersionKind(kind); validator.EmbeddedCRDs() // introspection; NewFromCRDs(docs...) for tests
```

Semantics, each taken from the apiserver source file named:

| Step | Validator | Apiserver source it mirrors |
|---|---|---|
| prune unknown fields, drop non-nullable nulls, coerce `metadata` | `normalize` | `pkg/apiserver/customresource_handler.go` `unstructuredSchemaCoercer.apply` |
| apply schema defaults (in place, written back into `obj`) | `normalize` → `structuraldefaulting.Default` | `unstructuredDefaulter` |
| create: ignore status (status subresource), clear deletion fields | `prepareCreate` | `rest.BeforeCreate`, `customResourceStrategy.PrepareForCreate` |
| update: keep old status; uid/creationTimestamp/generation/deletion\* from old | `prepareUpdate` | `rest.BeforeUpdate`, `PrepareForUpdate` |
| ObjectMeta (DNS-subdomain name, namespace iff namespaced, labels, annotations, immutable uid) | `metavalidation.ValidateObjectMetaDeclaratively` | `customResourceValidator.validateObjectMeta*` |
| OpenAPI schema (types, enums, patterns, required, min/max), **ratcheting on update** | `apiservervalidation.ValidateCustomResource[Update]` | `customResourceValidator.Validate[Update]` |
| embedded resources, list-type map/set | `schemaobjectmeta.Validate`, `structurallisttype.ValidateListSetsAndMaps` | `customResourceStrategy.Validate[Update]` |
| CEL incl. transition rules, skipped after "blocking" schema errors | `cel.Validator.Validate(..., RuntimeCELCostBudget, WithRatcheting)` | same, `hasBlockingErr` |
| status writes: spec/metadata from old, status schema + CEL on merged object | `ValidateStatus` | `statusStrategy.PrepareForUpdate/ValidateUpdate` |
| unknown fields in YAML/JSON → error (`kubectl --validate=strict`, the default) | `ValidateUnstructured` | strict field validation |

`Validate` does **not** drop or reset `obj.Status`. It only leaves status out of what it validates. Persisting the status rules (an Apply never changes status; UpdateStatus never changes spec) is the drivers' job; see "Recommended changes". `Validate` does not stamp uid or resourceVersion either (Apply contract item 3).

## Measurements

### Method

* Machine: 8 vCPU Intel Xeon @ 2.90 GHz, 16 GB, Linux 6.18, Go 1.27.2 linux/amd64, `CGO_ENABLED=0`.
* **Other agents were building in parallel.** The 1-minute load average was between 1.3 and 17 at the start of each build (peaks around 18), so build *times* are noisy. I alternated the variants every round so that load hit all of them alike, and I report the median of 3 rounds. Sizes and dependency counts are exact and do not vary between rounds.
* Every variant is HEAD (`326d9ae`) extracted with `git archive`, plus a measurement-only patch. None of the patches is committed. Scripts and patches are in the appendix.

| Variant | What cmd/kudurru links in addition to HEAD |
|---|---|
| `base` | nothing. Identical to the pre-spike HEAD `79a0e37` binary (11 813 024 B both ways) |
| `store` | `api/v1alpha1` scheme + `internal/store/memory`: the object model every store-backed build links anyway |
| `validator` | `internal/store/validator`: a hidden `spike-validator` command, and `validator.New()` called in `serve` |
| `kube` | controller-runtime v0.25.2 manager + client over the kudurru scheme: a stand-in for the kube driver |
| `kubeval` | `kube` + `validator`: the eventual single binary |

Build commands, per variant and round (`$T/src-$v` is the variant tree):

```sh
export CGO_ENABLED=0
GOCACHE=$(mktemp -d) go build -a -trimpath -ldflags "-s -w" -o kudurru-cold ./cmd/kudurru   # cold, empty cache
go build -a -trimpath -ldflags "-s -w" -o kudurru-a ./cmd/kudurru                           # cold, -a, shared cache
go build -trimpath -ldflags "-s -w" -o kudurru ./cmd/kudurru && rm kudurru
go build -trimpath -ldflags "-s -w" -o kudurru ./cmd/kudurru                                 # warm: cached compile, relink
stat -c %s kudurru
go version -m kudurru | grep -c $'^\tdep\t'
```

### Binary size and dependency count (exact)

| Variant | Bytes | MB | Δ vs base | Δ vs kube | Linked modules |
|---|---|---|---|---|---|
| `base` | 11 813 024 | 11.81 | +0.00 MB |  | 25 |
| `store` | 13 873 312 | 13.87 | +2.06 MB |  | 40 |
| `validator` | 24 711 328 | 24.71 | +12.90 MB |  | 69 |
| `kube` | 32 465 056 | 32.47 | +20.65 MB | +0.00 MB | 70 |
| `kubeval` | 39 133 344 | 39.13 | +27.32 MB | +6.67 MB | 81 |

The gate measures `validator` against `base`: **+12 898 304 B = 12.90 MB (12.30 MiB), +44 modules → PASS (< 25 MB).** There is also a cross-check: an earlier run imported the validator directly from `cmd/kudurru/main.go` in the worktree and gave 24 707 232 B (+12 894 208 B).

Where the bytes go, from `go tool nm -size` on an unstripped `validator` build (symbol bytes only, so it is not the full file size):

| Module | Symbol MB | Why it is linked |
|---|---|---|
| `k8s.io/api` | 1.78 | `k8s.io/apiserver/pkg/cel/library` (the Kubernetes CEL library that the CRD CEL environment registers) imports `authentication/serviceaccount`, which pulls in `core/v1`. The kube driver links this anyway. |
| `github.com/google/cel-go` | 1.09 | CEL compiler and interpreter |
| `google.golang.org/protobuf` | 0.84 | cel-go's value model |
| `k8s.io/apimachinery` | 0.48 | the object model links it anyway (see `store`) |
| `k8s.io/apiextensions-apiserver` | 0.42 | schema, CEL validator, defaulting, pruning, listtype, objectmeta |
| `k8s.io/kube-openapi` | 0.38 | the OpenAPI schema validator (`validate`, `strfmt`) |
| `k8s.io/apiserver` | 0.24 | CEL environment, feature gates |
| `antlr4-go`, `go-openapi/*`, `gnostic-models`, `component-base`, `otel` | ~0.5 | parser runtime, JSON pointer, OpenAPI models, metrics/feature registry, the trace API used by CEL |

There is **one CEL copy**: `github.com/google/cel-go v0.29.2` + `cel.dev/expr v0.25.1`, and no `github.com/cel-expr/cel-go`. apiextensions-apiserver v0.37.1 imports `github.com/google/cel-go v0.29.2`, not v0.31.x as §2 says. controller-runtime v0.25.2 does not link cel-go at all, so in the single binary the validator is the only CEL user until the OIDC claim mappings land (v0.2). Those must import the same module.

The licences of all 69 modules linked into `validator` are permissive: 43 Apache-2.0, 14 MIT, 12 BSD. No GPL, LGPL, MPL or EUPL module is linked.

### Build time

| Variant | Rounds | Cold, empty GOCACHE + `-a`: wall / CPU (s) | Cold `-a`, shared cache: wall / CPU (s) | Warm relink wall (s) | 1-min load at start |
|---|---|---|---|---|---|
| `base` | 3 | 10.7 / 47 | 11.7 / 46 | 0.34 | 1.31, 13.59, 16.99 |
| `store` | 3 | 12.2 / 59 | 13.2 / 59 | 0.44 | 4.43, 14.94, 14.66 |
| `validator` | 3 | 20.4 / 96 | 19.7 / 95 | 0.74 | 7.97, 11.41, 14.89 |
| `kube` | 3 | 41.8 / 207 | 41.9 / 206 | 1.37 | 5.02, 10.55, 15.58 |
| `kubeval` | 3 | 46.5 / 226 | 46.0 / 224 | 1.53 | 11.41, 7.97, 16.10 |

These are medians of 3 rounds. The second figure in each cell is CPU seconds (user + sys), which load affects less than wall time.

| Comparison | Cold build | Warm relink |
|---|---|---|
| `validator` vs `base` | +9.7 s wall, +49 CPU-s: about 2× | +0.4 s |
| `kubeval` vs `kube` | +4.7 s wall, +19 CPU-s: +9 % | +0.16 s |

The CI cost is a one-off cold compile of roughly 50 extra CPU-seconds on a fresh runner. A warm module and build cache removes it.

### Init time and RSS

`GODEBUG=inittrace=1 kudurru version` was run 7× per variant. The table shows the median sum of package-init clock time and allocations, and the max RSS of the process:

| Variant | Packages initialised | Init clock (median) | Init allocations | `version` max RSS | `serve` VmRSS after 2 s |
|---|---|---|---|---|---|
| base | 129 | 1.6 ms | 358 KiB | 10.0 MB | 12.2 MB |
| store | 163 | 2.2 ms | 580 KiB | 11.7 MB | 13.2–14.0 MB |
| validator | 246 | 7.1 ms | 2.2 MiB | 18.8–20.5 MB | 27.0–27.5 MB (validator built) |
| kube | 315 | ~20–30 ms (noisy) | 3.0 MiB | 26.1 MB | – |
| kubeval | 373 | ~30 ms (noisy) | 4.4 MiB | 32.0 MB | – |

The largest initialisers in `validator`:

| Package | Init clock | Allocated |
|---|---|---|
| `k8s.io/component-base/metrics/legacyregistry` | 3.1 ms | 269 KB |
| `k8s.io/kube-openapi/pkg/validation/strfmt` | 1.2 ms | 436 KB |
| `google.golang.org/protobuf/reflect/protodesc` | 1.1 ms | 251 KB |
| `cel-go/common/stdlib` | 0.36 ms | – |
| `k8s.io/apiserver/pkg/features` | 0.21 ms | – |

All of these run whether or not the validator is ever constructed.

First use, from the hidden `kudurru spike-validator` command (3 runs):

```
validator.New():                                   13.9 ms | 20.6 ms | 14.2 ms   (RSS +5–6 MB: compiled CEL programs, schemas)
first Validate (create, DNSZone):                   1.07 ms | 0.45 ms | 0.45 ms
second Validate (create):                           0.12 ms | 0.39 ms | 0.13 ms
update rejected by `self == oldSelf`:               0.22 ms | 1.04 ms | 0.24 ms
steady-state Validate (update, CEL transition):     218 µs/op | 225 µs/op | 269 µs/op   (2000 ops)
RSS after 2000 validations: 28.4–29.4 MB; Go heap in use 6.7–7.7 MiB
`serve` with validator.New() at startup: "spike: validator.New() took 12.2 ms | 12.5 ms | 12.7 ms"
```

The steady-state cost is about 0.2 ms of CPU per control-plane write, which is irrelevant next to a SQLite transaction. Validation never runs on the query path.

## Fidelity: what "identical to the apiserver" means here, and how it is tested

Every decision is delegated to the same apiextensions/apiserver/apimachinery functions the apiserver calls, at the same versions (v0.37.1), with the same feature-gate defaults: CRDValidationRatcheting (GA, locked on) and DeclarativeValidationBeta. The only kudurru logic is the call sequence and the status/identity preparation. Each of those is cited to its apiserver source above.

Tests (`go test -race ./internal/store/validator`: 27 top-level tests, 130 cases including subtests):

* **Every CRD has a valid fixture.** The `config/samples/*.yaml` files are decoded strictly into the Go types and pass on both the typed and the unstructured path.
* **38 invalid variants, each asserting the exact field path and cause type**, checked on create and again on update. They cover:
  * zoneName pattern (no trailing dot, space);
  * every enum (zone type, queryAccess, notify, transferProtocol, dnssec algorithm, publish policy, list format/mode/response, blockMode);
  * required fields (targets, endpoints, sources, primaries, stub nameservers), `minItems` (targets, sources) and `maxItems` 1000 (endpoints);
  * recordType and schedule patterns;
  * the **BlockList source one-of** (url+inline, none, all three, second item) and the **ClientGroup matcher one-of** (ip+cidr, name only, mac+hostname);
  * metadata: invalid DNS-subdomain name, missing name, namespace on the cluster-scoped DNSZone, missing namespace on namespaced kinds, bad label key.
* **Transition rules.** Changing `zoneName` on update is rejected with `zoneName is immutable`; the same value with other changes is accepted; changing it on create is accepted. A typed-nil `old` counts as a create. A changed `zoneRef` (name, or a namespace added) is rejected; adding an endpoint is accepted.
* **Ratcheting.** A stored object with a now-invalid enum value can be updated as long as that value is unchanged. Changing it to another invalid value, or creating with it, is rejected. The same holds for status conditions.
* **Defaults.**
  * Typed: SOA refresh/retry/expire/minimum = 3600/900/1209600/300, `defaultTTL` 300, policy access modes, DNSSEC algorithm/denial. Explicit values are preserved.
  * Unstructured: `blockTTL` 10m, `refreshInterval` 24h, `mode` block, `format` auto, `schedule.tz` UTC, `forward.upstreamGroup` default.
  * Both entry points produce semantically equal objects for every sample.
* **Status.** A main-resource write ignores even an invalid status. `ValidateStatus` enforces the `metav1.Condition` schema (reason pattern, status enum) and `listType=map` uniqueness on `conditions[].type`, and ignores spec changes.
* **Unstructured path.** Unknown fields are rejected (strict). A string where an integer belongs gives `TypeInvalid`, which also stops CEL as on the apiserver. Wrong apiVersion, unknown kinds and malformed metadata are rejected, and a rejected input is left untouched.
* **Errors.**
  * The message, `Status()` and causes equal `apierrors.NewInvalid` for the same field errors.
  * `FromStatusError` round-trips an apiserver 422 into the same `*Error`.
  * A golden file (`testdata/errors.golden`) pins the API envelope for representative rejections.
* **CRDs.** A drift test fails if `crds/` differs from `config/crd/bases`. **`TestEmbeddedCRDsInstall` runs the apiserver's own `ValidateCustomResourceDefinition`** (structural schema, pruned defaults, CEL cost estimation) over every CRD; see Surprise 1.
* **Concurrency.** 8 goroutines × 50 iterations share one `Validator` under `-race`.

What is deliberately not emulated, or is outside the validator's reach:

* Sub-object field ownership: server-side-apply managedFields are not emulated, by design (§4.3 item 2).
* Admission webhooks: none are configured (`webhook.enabled=false`).
* The apiserver request cap (3 MiB) and the etcd object cap (1.5 MiB by default). The sqlite driver should enforce the 1.5 MiB cap itself (risk #13); see the follow-ups.
* CEL compatibility. The local CEL environment is the v0.37 library's `DefaultCompatibilityVersion`. A cluster running an older Kubernetes rejects rules that use newer CEL libraries when the **CRD** is installed. `TestEmbeddedCRDsInstall` catches this only for the v0.37 environment. Keep rules within the CEL libraries of the oldest supported Kubernetes minor.

## Fallback prototype (risk #10), for comparison only

This prototype is not needed, because the gate passed. I built `cmd/kudurru spike-fallback CRD OBJECT` on HEAD. It uses `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3 (Apache-2.0), which validates the CRD's `openAPIV3Schema` as JSON Schema 2020-12 and ignores the `x-kubernetes-*` keywords. It also uses `github.com/google/cel-go` v0.29.2 directly, with `self`/`oldSelf` declared as `dyn`, and evaluates every x-kubernetes-validations rule on the matching nodes.

Result:

```
$ bash .spike-tools/measure-fallback.sh
fallback size=15937696 deps=33          # base: 11813024 B, 25 modules  ->  +4.12 MB, +8 modules
  new modules: cel.dev/expr, antlr4-go/antlr/v4, google/cel-go v0.29.2, santhosh-tekuri/jsonschema/v6 v6.0.3,
               golang.org/x/exp, golang.org/x/text, genproto/googleapis/{api,rpc}
--- valid sample
done
--- invalid (enum + one-of)
schema: jsonschema validation failed with 'file:///.../crd.json#'
- at '/spec/mode': value must be one of 'block', 'allow'
cel: spec.sources[0]: exactly one of url, inline or configMapRef must be set
--- version RSS: 15.5–16.0 MB (base 10.0 MB, full validator 19–20.5 MB)

$ bash .spike-tools/setup-kubefb.sh      # kube variant + fallback instead of the validator
kubefb size=36352160 deps=77             # kube: 32465056 B  ->  +3.89 MB (full validator: +6.67 MB)
```

| | Scaffold binary (vs `base`) | Single binary with controller-runtime (vs `kube`) |
|---|---|---|
| Full validator (apiextensions + CEL) | +12.90 MB | +6.67 MB |
| Fallback (JSON Schema + cel-go) | +4.12 MB | +3.89 MB |
| Saved by the fallback | 8.78 MB | **2.78 MB** |

What the fallback loses (each item is a source of drift between Docker and Kubernetes):

* no structural defaulting, pruning, or null handling;
* no validation ratcheting;
* no list-type map/set checks;
* no ObjectMeta validation;
* no Kubernetes CEL libraries (`quantity`, `url`, `ip`, `cidr`, `format`, `isSorted`, ...) and no CEL cost limits;
* no `oldSelf` optional semantics;
* untyped `dyn` CEL environment instead of the schema-typed one, so type errors surface at runtime instead of compile time;
* error messages differ from the apiserver's.

Each of those would need re-implementing and keeping in sync by hand. That is the "two validators" alternative D34 rejected, and saving 2.8 MB in the real binary does not justify it.

## Surprises

1. **Two of the five CRDs cannot be installed on Kubernetes today.** The apiserver's CRD create validation refuses `kudurru.io_blocklists.yaml` and `kudurru.io_clientgroups.yaml`:
   ```
   spec.validation.openAPIV3Schema.properties[spec].properties[sources].items.x-kubernetes-validations[0].rule:
     Forbidden: estimated rule cost exceeds budget by factor of 7.1x (try simplifying the rule, or adding maxItems, ...)
   spec.validation.openAPIV3Schema.properties[spec].properties[clients].items.x-kubernetes-validations[0].rule:
     Forbidden: estimated rule cost exceeds budget by factor of 11.8x ...
   spec.validation.openAPIV3Schema: Forbidden: x-kubernetes-validations estimated rule cost total for entire OpenAPIv3 schema exceeds budget by factor of 1.184891x
   ```
   * **Cause:** the one-of rules sit on items of lists without `maxItems`, so the estimator assumes the largest list a 3 MiB request can hold.
   * **Fix:** add `+kubebuilder:validation:MaxItems=` to `BlockListSpec.Sources` and `ClientGroupSpec.Clients`. I probed values from 8 to 100 000: any value ≤ 65 536 installs, and 100 000 is still refused for `clients`.
   * **Suggested limits:** `Sources` ≤ 64 and `Clients` ≤ 1000. Use the same bound as the 1000 endpoints for `DNSRecordSet`.
   * **Tests:** `knownUninstallable` in `crds_test.go` pins both refusals and fails once they are fixed, so the entry gets removed. `TestKnownUninstallableFix` proves `maxItems: 1000` passes.
   * Local Docker mode would never have noticed this, because the runtime cost is per actual item.
2. **Typed objects defeat some schema defaults, on the apiserver too.**
   * `BlockListSpec.RefreshInterval` and `PolicySpec.BlockTTL` are non-pointer `metav1.Duration` with `omitempty`. encoding/json never omits a struct, so a typed client sends `"0s"` and the 24h / 10m defaults never apply.
   * `ForwardSpec.UpstreamGroup` has a default but no `omitempty`, so a typed client always sends `""`.
   * The validator reproduces this faithfully. `TestTypedDurationZeroIsNotDefaulted` pins it, and the defaults hold on the unstructured path.
   * Verified fix: with `json:"blockTTL,omitzero"`, both `encoding/json` and `runtime.DefaultUnstructuredConverter` omit the zero value. The probe output was `{"ttl":"0s"}` with omitempty and `{}` with omitzero, and likewise for the unstructured converter.
   * The same gap explains why the API should validate request bodies as unstructured before typing them (follow-ups).
3. **Schema gaps accepted by both modes.** These are pinned in `TestSchemaGaps` so that tightening a marker flips the test:
   * `zoneName: ""`: `^([a-zA-Z0-9_-]+\.)*$` matches the empty string.
   * `zoneRef.name: ""` and `dnsName: ""`: there is no MinLength, and the field has no omitempty, so `required` is satisfied.
   * `endpoints: []`.
   * `response: customIP` without `customIP`.
4. **cel-go version.** apiextensions-apiserver v0.37.1 imports `github.com/google/cel-go` **v0.29.2**, not the v0.31.x that §2 says. The module path is still `github.com/google/cel-go`.
5. **RSS grows even if the validator is never used.** Linking it raises `kudurru version` from 10 MB to about 19–20 MB, from package inits and the larger touched text/rodata. In `validator` the biggest initialiser is `component-base/metrics/legacyregistry`, which builds a Prometheus registry at init. Constructing the validator adds another 5–6 MB. In the single binary with controller-runtime the marginal cost is +6 MB at `version`.
6. **The memory reference driver keeps `status` on Apply.** The apiserver ignores status on main-resource writes when the status subresource exists. This is risk #9 drift waiting to happen. It belongs in the conformance suite, not in the validator.
7. Metadata update validation requires `resourceVersion` and forbids decrementing `generation`. The validator mirrors `rest.BeforeUpdate`: generation, uid, creationTimestamp and deletion fields come from the stored object. A placeholder resourceVersion is used only in the internal validation view, because concurrency is the driver's `ErrConflict`.
8. Faithful-but-ugly apiserver output. A blocking schema error adds a cause `<nil>: Invalid value: null: some validation rules were not checked...`. `Fields()` and `Hint()` drop it; the message keeps it, as kubectl shows it. CEL errors on objects in 1.37 render as `Invalid value: <rule message>` without the old `"object"` value.
9. Process issues:
   * This worktree was created at the initial commit `5a893a4`, not at the scaffold. I fast-forwarded the branch to `79a0e37` before starting; there was no rebase and no branch switch.
   * `make test` exports `CGO_ENABLED=0` and runs `go test -race`, which fails with "-race requires cgo". This was already broken in the scaffold. I ran the tests with `CGO_ENABLED=1 go test -race ./...`.

## Recommended changes

To `docs/architecture.md` (I did not edit it):

* **§2, CEL row:** "`github.com/google/cel-go` **v0.29.2** (the version `k8s.io/apiextensions-apiserver` v0.37.1 imports; bump only together with the Kubernetes minor). Linked only by `internal/store/validator` today; the OIDC claim mappings (v0.2) must use the same module."
* **§14, risk #10:** mark it retired. "Measured in spike (g): +12.9 MB on the scaffold binary, +6.7 MB in a binary that already links controller-runtime; RSS +9 MB at start (+15 MB with the validator built); fallback prototype +3.9 MB in the single binary (saves 2.8 MB) with far lower fidelity. Mitigation going forward: CI binary-size budget." Keep the seam.
* **§4.3, Apply contract item 4:** name the entry points and the split of responsibilities.
  * `store.Validator` exposes `Validate` (Apply), `ValidateStatus` (UpdateStatus) and `ValidateUnstructured` (API/file input).
  * The validator applies defaults. Drivers persist the defaulted object.
  * Drivers own status persistence: Apply never changes status, UpdateStatus never changes spec/metadata.
  * The kube driver maps apiserver 422s with `validator.FromStatusError`, so both modes return one error type, which the API maps to `400 invalid`.
* **§4.1, after "Validation is CRD OpenAPI + CEL first":** add API conventions, enforced by `TestEmbeddedCRDsInstall`:
  1. Every list whose items (or which itself) carries x-kubernetes-validations needs `maxItems`, or the CRD will not install.
  2. Optional struct-typed fields with defaults use `omitzero`.
  3. Name and reference strings get `MinLength=1`.
* **§10 / §4.3:** the API decodes request bodies to unstructured, calls `ValidateUnstructured` (strict unknown-field rejection and complete defaulting), and only then converts to the Go type.
* **§16:** add a decision row: "Store validator writes defaults back; status reset is a driver duty; API validates unstructured first" (spike g). The alternative it rejects: validating typed objects only, which loses absent-vs-zero.

To code owned by other agents (follow-ups):

* `api/v1alpha1`:
  * `MaxItems` on `BlockListSpec.Sources` (64) and `ClientGroupSpec.Clients` (1000);
  * `omitzero` on `RefreshInterval` and `BlockTTL`;
  * `omitempty` (or `+optional`) on `ForwardSpec.UpstreamGroup`;
  * `MinLength=1` on `DNSZoneSpec.ZoneName`, `ObjectRef.Name` and `Endpoint.DNSName`;
  * a CEL rule `self.response != 'customIP' || has(self.customIP)`.

  Then run `make generate`. That updates `crds/`, and `knownUninstallable` and `TestSchemaGaps` tell you what to delete.
* `internal/store/store.go`: add `type Validator interface { Validate(ctx, obj, old Object) error; ValidateStatus(ctx, obj, old Object) error }` so drivers do not import the heavy package directly. Have the sqlite and memory drivers call it inside `Apply`/`UpdateStatus` before writing.
* `internal/store/conformance`:
  * a status-semantics case (Apply ignores status, UpdateStatus ignores spec);
  * a shared "rejects the same inputs" table driven by the invalid fixtures, run against sqlite and kube (envtest).
* sqlite driver / API: enforce the 1.5 MiB object cap that etcd enforces.
* Makefile: run `go test -race` with `CGO_ENABLED=1`; keep `CGO_ENABLED=0` for `build`.
* CI: add a binary-size budget step that compares `go build -trimpath -ldflags "-s -w"` against a checked-in ceiling.

## Appendix: exact scripts and measurement patches

All of this lives outside the repository (in the worktree's untracked `.spike-tools/`). It is reproduced here so the numbers can be regenerated.

### `setup-src.sh`

Variant trees (base/store/validator/kube/kubeval) from `git archive --format=tar -o .spike-tools/head.tar HEAD`.

```sh
#!/usr/bin/env bash
# setup-src.sh — extract HEAD into one source tree per measured variant and
# apply the measurement-only patches (never committed).
#   base      HEAD as committed (cmd does not import the validator)
#   store     + api/v1alpha1 scheme and the memory store (object model cost)
#   validator + internal/store/validator (spike-validator cmd, New() in serve)
#   kube      + controller-runtime manager/client (kube driver proxy)
#   kubeval   kube + validator
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
P=$T/patches
export PATH=/usr/local/go/bin:$PATH
CR_VERSION=${CR_VERSION:-v0.25.2}
for d in base store validator kube kubeval; do
  rm -rf "$T/src-$d"
  mkdir -p "$T/src-$d"
  tar -xf "$T/head.tar" -C "$T/src-$d"
done
for d in store validator kube kubeval; do
  cp "$P/spike_registry.go" "$T/src-$d/cmd/kudurru/"
  (cd "$T/src-$d" && patch -p1 -s < "$P/cmd-registry.diff")
done
cp "$P/spike_store.go" "$T/src-store/cmd/kudurru/"
for d in validator kubeval; do
  cp "$P/spike_validator.go" "$T/src-$d/cmd/kudurru/"
  (cd "$T/src-$d" && patch -p1 -s < "$P/cmd-serve.diff")
done
for d in kube kubeval; do
  cp "$P/spike_kube.go" "$T/src-$d/cmd/kudurru/"
  (cd "$T/src-$d" && go get "sigs.k8s.io/controller-runtime@${CR_VERSION:?}" >/dev/null 2>&1 && go mod tidy >/dev/null 2>&1)
done
echo ok
```

### `measure2.sh`

Size, dependency count and build times; run as `bash .spike-tools/measure2.sh 3 base store validator kube kubeval`.

```sh
#!/usr/bin/env bash
# measure2.sh ROUNDS VARIANT... — release-style builds of each variant tree
# (src-<variant>, see setup-src.sh), alternating variants every round so
# machine load (other agents build in parallel) hits all variants alike.
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
RS=$T/bin/runstat
export PATH=/usr/local/go/bin:$PATH CGO_ENABLED=0
rounds=$1
shift
for r in $(seq 1 "$rounds"); do
  for v in "$@"; do
    src=$T/src-$v
    out=$T/out2/$v
    mkdir -p "$out"
    cd "$src"
    load=$(cut -d' ' -f1-3 /proc/loadavg)
    cache=$T/cache-$v-$r
    rm -rf "$cache"
    cold=$(GOCACHE=$cache $RS go build -a -trimpath -ldflags "-s -w" -o "$out/kudurru-cold" ./cmd/kudurru | tail -1)
    rm -rf "$cache"
    colda=$($RS go build -a -trimpath -ldflags "-s -w" -o "$out/kudurru-a" ./cmd/kudurru | tail -1)
    go build -trimpath -ldflags "-s -w" -o "$out/kudurru" ./cmd/kudurru
    rm -f "$out/kudurru"
    warm=$($RS go build -trimpath -ldflags "-s -w" -o "$out/kudurru" ./cmd/kudurru | tail -1)
    size=$(stat -c %s "$out/kudurru")
    go version -m "$out/kudurru" > "$out/deps.txt"
    deps=$(grep -c $'^\tdep\t' "$out/deps.txt")
    echo "$v r$r load[$load] size=$size deps=$deps | cold(fresh GOCACHE,-a) $cold | cold(-a) $colda | warm(relink) $warm"
  done
done
```

### `inittrace.sh`

Package init time and `version` RSS; run as `bash .spike-tools/inittrace.sh .spike-tools/out2/<variant>/kudurru 7`.

```sh
#!/usr/bin/env bash
# inittrace.sh BINARY [RUNS] — total package-init clock time and allocations
# (GODEBUG=inittrace=1) per run of `BINARY version`, plus max RSS.
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
bin=$1
runs=${2:-7}
for i in $(seq 1 "$runs"); do
  GODEBUG=inittrace=1 "$bin" version 2>"$T/it.txt" >/dev/null
  init=$(awk '{t+=$5; a+=$8; n++} END {printf "init=%.2fms alloc=%dKiB pkgs=%d", t, a/1024, n}' "$T/it.txt")
  rss=$("$T/bin/runstat" "$bin" version 2>/dev/null | sed -E 's/.*maxrss=([0-9]+KB).*/\1/')
  echo "$init version-maxrss=$rss"
done
```

### `runstat/main.go`

`runstat`: wall/CPU/max-RSS of a child process (no /usr/bin/time on the box); `runstat -sample 2s kudurru serve --config ...` reads VmRSS.

```go
// runstat runs a command and prints wall time, CPU time and max RSS.
// With -sample D it starts the command, waits D, reads VmRSS/VmHWM from
// /proc and kills it (for long-running servers).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func main() {
	sample := flag.Duration("sample", 0, "sample RSS after this delay and kill")
	flag.Parse()
	args := flag.Args()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = os.Environ()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *sample > 0 {
		time.Sleep(*sample)
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
				fmt.Println(strings.Join(strings.Fields(l), " "))
			}
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	err := cmd.Wait()
	wall := time.Since(start)
	ru := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	fmt.Printf("wall=%.2fs user=%.2fs sys=%.2fs maxrss=%dKB exit=%v\n", wall.Seconds(),
		time.Duration(ru.Utime.Nano()).Seconds(), time.Duration(ru.Stime.Nano()).Seconds(), ru.Maxrss, err)
}
```

### `sizes.py`

Symbol bytes per module: `go tool nm -size kudurru-syms > nm.txt && python3 -I sizes.py nm.txt deps.txt`.

```python
"""sizes.py NMFILE DEPSFILE — sum `go tool nm -size` symbol bytes per module.

Symbols are attributed to the longest module path (from `go version -m`)
that prefixes the symbol's package path; everything else is "std/runtime".
"""
import re
import sys

nm_file, deps_file = sys.argv[1], sys.argv[2]
mods = ["github.com/robertherbaugh/kudurru"]
for line in open(deps_file):
    parts = line.split()
    if len(parts) >= 3 and parts[0] == "dep":
        mods.append(parts[1])
mods.sort(key=len, reverse=True)

sizes = {}
total = 0
for line in open(nm_file):
    parts = line.split(None, 3)
    if len(parts) < 4:
        continue
    try:
        size = int(parts[1])
    except ValueError:
        continue
    if parts[2] in ("B", "b", "U"):
        continue  # bss and undefined symbols take no file space
    name = parts[3].strip()
    name = re.sub(r"^(type:\.?(eq\.)?|go:itab\.|go:string\.|go:)", "", name)
    name = name.lstrip("*").lstrip("(").lstrip("*")
    key = "std/runtime/other"
    for m in mods:
        if name.startswith(m + ".") or name.startswith(m + "/"):
            key = m
            break
    sizes[key] = sizes.get(key, 0) + size
    total += size

for k, v in sorted(sizes.items(), key=lambda kv: -kv[1])[:30]:
    print(f"{v/1e6:8.2f} MB  {k}")
print(f"{total/1e6:8.2f} MB  total symbol bytes")
```

### `licenses.sh`

Licence sniffing of every linked module: `bash licenses.sh deps.txt`.

```sh
#!/usr/bin/env bash
# licenses.sh DEPSFILE — print a licence guess for every module linked into a
# binary (from `go version -m` output), by sniffing its LICENSE file.
set -euo pipefail
M=/home/vercel-sandbox/go/pkg/mod
grep -P '^\tdep\t' "$1" | while read -r _ mod ver _; do
  esc=$(echo "$mod" | sed -E 's/([A-Z])/!\L\1/g')
  dir="$M/$esc@$ver"
  f=$(ls "$dir"/LICENSE* "$dir"/COPYING* 2>/dev/null | head -1 || true)
  lic="?"
  if [ -n "$f" ]; then
    if grep -qi 'GNU GENERAL PUBLIC\|GNU LESSER\|EUPL\|Mozilla Public' "$f"; then lic="COPYLEFT!"
    elif grep -qi 'Apache License' "$f"; then lic="Apache-2.0"
    elif grep -qi 'Permission is hereby granted, free of charge' "$f"; then lic="MIT"
    elif grep -qi 'Redistribution and use in source and binary forms' "$f"; then lic="BSD"
    else lic="other"; fi
  fi
  echo "$lic $mod $ver"
done | sort
```

### `kudurru.yaml`

Config for the `serve` RSS samples (ports chosen not to collide with parallel agents).

```yaml
# Spike-only config: unusual ports so parallel agents do not collide.
node:
  name: spike
  dataDir: /home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools/data
listeners:
  dns:
    - address: "127.0.0.1:25353"
      protocols: [udp, tcp]
  api: { address: "127.0.0.1:28080" }
  ops: { address: "127.0.0.1:28081" }
  metrics: { address: "127.0.0.1:29153" }
upstreams:
  groups:
    default:
      servers: ["1.1.1.1:53"]
      strategy: parallel_best
      timeout: 2s
log:
  level: info
  format: text
```

### `setup-fallback.sh`

Fallback prototype tree.

```sh
#!/usr/bin/env bash
# setup-fallback.sh — src-fallback: HEAD + the risk #10 fallback prototype
# (santhosh-tekuri/jsonschema/v6 + cel-go directly, no apiextensions).
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
P=$T/patches
export PATH=/usr/local/go/bin:$PATH
rm -rf "$T/src-fallback"
mkdir -p "$T/src-fallback"
tar -xf "$T/head.tar" -C "$T/src-fallback"
cp "$P/spike_registry.go" "$P/spike_fallback.go" "$T/src-fallback/cmd/kudurru/"
cd "$T/src-fallback"
patch -p1 -s < "$P/cmd-registry.diff"
go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3 github.com/google/cel-go@v0.29.2 >/dev/null 2>&1
go mod tidy >/dev/null 2>&1
grep -E 'jsonschema|cel-go' go.mod
echo ok
```

### `measure-fallback.sh`

Fallback size, linked-module delta and smoke test.

```sh
#!/usr/bin/env bash
# measure-fallback.sh — size and smoke-test the risk #10 fallback prototype.
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
RS=$T/bin/runstat
export PATH=/usr/local/go/bin:$PATH CGO_ENABLED=0
out=$T/out2/fallback
mkdir -p "$out"
cd "$T/src-fallback"
$RS go build -trimpath -ldflags "-s -w" -o "$out/kudurru" ./cmd/kudurru | tail -1
size=$(stat -c %s "$out/kudurru")
go version -m "$out/kudurru" > "$out/deps.txt"
echo "fallback size=$size deps=$(grep -c $'^\tdep\t' "$out/deps.txt")"
grep -P '^\tdep\t' "$T/out2/base/deps.txt" | awk '{print $2}' > "$T/base.mods"
grep -P '^\tdep\t' "$out/deps.txt" | awk '{print $2, $3}' | grep -v -F -f "$T/base.mods" || true
echo "--- valid sample"
"$out/kudurru" spike-fallback config/crd/bases/kudurru.io_blocklists.yaml config/samples/kudurru.io_v1alpha1_blocklist.yaml
echo "--- invalid (enum + one-of)"
"$out/kudurru" spike-fallback config/crd/bases/kudurru.io_blocklists.yaml "$T/bad-blocklist.yaml"
echo "--- version RSS"
for i in 1 2 3; do $RS "$out/kudurru" version 2>/dev/null; done
```

### `bad-blocklist.yaml`

Invalid object for the fallback smoke test.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata:
  name: bad
  namespace: dns
spec:
  mode: deny
  sources:
    - url: https://a.example/
      inline: [ads.example]
```

### `summarize.py`

Median tables from measure2.log: `python3 -I summarize.py measure2.log`.

```python
"""summarize.py LOG — markdown tables (medians over rounds) from measure2.log."""
import re
import statistics
import sys

rows = {}
order = []
pat = re.compile(
    r"^(\S+) r(\d+) load\[([^\]]*)\] size=(\d+) deps=(\d+) \| "
    r"cold\(fresh GOCACHE,-a\) wall=([\d.]+)s user=([\d.]+)s sys=([\d.]+)s.*?\| "
    r"cold\(-a\) wall=([\d.]+)s user=([\d.]+)s sys=([\d.]+)s.*?\| "
    r"warm\(relink\) wall=([\d.]+)s user=([\d.]+)s sys=([\d.]+)s"
)
for line in open(sys.argv[1]):
    m = pat.match(line.strip())
    if not m:
        continue
    v = m.group(1)
    if v not in rows:
        rows[v] = []
        order.append(v)
    g = m.groups()
    rows[v].append({
        "load": g[2].split()[0], "size": int(g[3]), "deps": int(g[4]),
        "cold_wall": float(g[5]), "cold_cpu": float(g[6]) + float(g[7]),
        "a_wall": float(g[8]), "a_cpu": float(g[9]) + float(g[10]),
        "warm_wall": float(g[11]),
    })

def med(v, k):
    return statistics.median(r[k] for r in rows[v])

base = rows["base"][0]["size"]
kube = rows["kube"][0]["size"] if "kube" in rows else None
print("| Variant | Bytes | MB | Δ vs base | Δ vs kube | Linked modules |")
print("|---|---|---|---|---|---|")
for v in order:
    s = rows[v][0]["size"]
    assert all(r["size"] == s for r in rows[v]), v
    dk = f"{(s - kube)/1e6:+.2f} MB" if kube and v.startswith("kube") else ""
    print(f"| `{v}` | {s:,} | {s/1e6:.2f} | {(s-base)/1e6:+.2f} MB | {dk} | {rows[v][0]['deps']} |".replace(",", " "))
print()
print(f"| Variant | Rounds | Cold, empty GOCACHE + `-a`: wall / CPU (s) | Cold `-a`, shared cache: wall / CPU (s) | Warm relink wall (s) | 1-min load at start |")
print("|---|---|---|---|---|---|")
for v in order:
    loads = ", ".join(r["load"] for r in rows[v])
    print(f"| `{v}` | {len(rows[v])} | {med(v,'cold_wall'):.1f} / {med(v,'cold_cpu'):.0f} | {med(v,'a_wall'):.1f} / {med(v,'a_cpu'):.0f} | {med(v,'warm_wall'):.2f} | {loads} |")
```

### `patches/spike_registry.go`

Patch: registry for hidden measurement commands.

```go
package main

import "github.com/spf13/cobra"

// spikeCmds collects measurement-only hidden commands (spike g).
var spikeCmds []func() *cobra.Command
```

### `patches/cmd-registry.diff`

Patch: main.go hook.

```diff
--- a/cmd/kudurru/main.go
+++ b/cmd/kudurru/main.go
@@ -43,6 +43,9 @@ func newRoot() *cobra.Command {
 		newDoctorCmd(&configPath),
 		newConfigCmd(&configPath),
 	)
+	for _, f := range spikeCmds {
+		root.AddCommand(f())
+	}
 	return root
 }
```

### `patches/cmd-serve.diff`

Patch: build the validator at `serve` start (validator/kubeval).

```diff
diff --git a/cmd/kudurru/serve.go b/cmd/kudurru/serve.go
index 0ec9f11..746c25e 100644
--- a/cmd/kudurru/serve.go
+++ b/cmd/kudurru/serve.go
@@ -45,6 +45,11 @@ milestone (docs/architecture.md §13).`,
 			}
 			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
 			defer stop()
+			if _, d, err := spikeBuildValidator(); err != nil {
+				return err
+			} else {
+				fmt.Fprintln(os.Stderr, "spike: validator.New() took", d)
+			}
 			return runServe(ctx, cfg)
 		},
 	}
```

### `patches/spike_validator.go`

Patch: `kudurru spike-validator` (init time, first/steady Validate, RSS).

```go
package main

// TEMPORARY, measurement only (spike g, docs/spikes/store-validator.md).
// Not committed: it links internal/store/validator into the binary the way
// the sqlite driver will, so size, init time and RSS can be measured.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var spikeStart = time.Now()

func spikeRSS() string {
	b, _ := os.ReadFile("/proc/self/status")
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS") || strings.HasPrefix(l, "VmHWM") {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
	}
	return strings.Join(out, " ")
}

func spikeZone(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary,
			SOA:         &v1alpha1.SOASpec{MName: "ns1." + zoneName, RName: "hostmaster." + zoneName},
			Nameservers: []string{"ns1." + zoneName},
		},
	}
}

// spikeBuildValidator is what the sqlite driver would do at startup.
func spikeBuildValidator() (*validator.Validator, time.Duration, error) {
	t0 := time.Now()
	v, err := validator.New()
	return v, time.Since(t0), err
}

func init() { spikeCmds = append(spikeCmds, newSpikeValidatorCmd) }

func newSpikeValidatorCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "spike-validator",
		Hidden: true,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := context.Background()
			sinceStart := time.Since(spikeStart)
			rss0 := spikeRSS()
			v, tNew, err := spikeBuildValidator()
			if err != nil {
				return err
			}
			rss1 := spikeRSS()
			t1 := time.Now()
			if err := v.Validate(ctx, spikeZone("home", "home.example."), nil); err != nil {
				return err
			}
			tFirst := time.Since(t1)
			t2 := time.Now()
			if err := v.Validate(ctx, spikeZone("lab", "lab.example."), nil); err != nil {
				return err
			}
			tSecond := time.Since(t2)
			old := spikeZone("home", "home.example.")
			old.ResourceVersion, old.UID, old.Generation = "1", "u1", 1
			upd := spikeZone("home", "other.example.")
			t3 := time.Now()
			errUpd := v.Validate(ctx, upd, old)
			tUpd := time.Since(t3)
			const n = 2000
			t4 := time.Now()
			for i := 0; i < n; i++ {
				_ = v.Validate(ctx, spikeZone("home", "home.example."), old)
			}
			per := time.Since(t4) / n
			rss2 := spikeRSS()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			fmt.Printf("process start -> main: %v (rss %s)\n", sinceStart, rss0)
			fmt.Printf("validator.New(): %v (rss after %s)\n", tNew, rss1)
			fmt.Printf("first Validate (create): %v\n", tFirst)
			fmt.Printf("second Validate (create): %v\n", tSecond)
			fmt.Printf("update with immutable zoneName change: %v (rejected=%v)\n", tUpd, validator.IsInvalid(errUpd))
			fmt.Printf("steady-state Validate (update, CEL transition rule): %v/op over %d\n", per, n)
			fmt.Printf("rss after %d validations: %s; heap in use %d KiB\n", n, rss2, ms.HeapInuse/1024)
			return nil
		},
	}
}
```

### `patches/spike_store.go`

Patch: object-model baseline (`store`).

```go
package main

// TEMPORARY, measurement only (spike g): links what every store-backed build
// links anyway (api/v1alpha1, apimachinery, the memory store), so the
// validator's own cost can be separated from the object model's.

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func init() { spikeCmds = append(spikeCmds, newSpikeStoreCmd) }

func newSpikeStoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "spike-store",
		Hidden: true,
		RunE: func(c *cobra.Command, _ []string) error {
			s := runtime.NewScheme()
			if err := v1alpha1.AddToScheme(s); err != nil {
				return err
			}
			st := memory.New()
			z := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary}}
			out, err := st.Apply(context.Background(), z, store.ApplyOptions{Manager: store.ManagerAPI})
			if err != nil {
				return err
			}
			fmt.Println(store.KeyOf(out), len(s.AllKnownTypes()))
			return nil
		},
	}
}
```

### `patches/spike_kube.go`

Patch: controller-runtime stand-in for the kube driver (`kube`, `kubeval`; `go get sigs.k8s.io/controller-runtime@v0.25.2`).

```go
package main

// TEMPORARY, measurement only (spike g): links a controller-runtime manager
// and client over the kudurru scheme, roughly what the kube driver adds to
// the single binary, so the validator's marginal cost in the final product
// can be measured.

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func init() { spikeCmds = append(spikeCmds, newSpikeKubeCmd) }

func newSpikeKubeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "spike-kube",
		Hidden: true,
		RunE: func(c *cobra.Command, _ []string) error {
			s := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(s); err != nil {
				return err
			}
			if err := v1alpha1.AddToScheme(s); err != nil {
				return err
			}
			cfg := &rest.Config{Host: "https://127.0.0.1:6443"}
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: s, LeaderElection: true, LeaderElectionID: "kudurru", LeaderElectionNamespace: "kudurru"})
			if err != nil {
				return err
			}
			cl, err := client.New(cfg, client.Options{Scheme: s})
			if err != nil {
				return err
			}
			fmt.Println(mgr != nil, cl != nil)
			return nil
		},
	}
}
```

### `patches/spike_fallback.go`

Patch: risk #10 fallback prototype.

```go
package main

// TEMPORARY, measurement only (spike g, risk #10 fallback): validate a
// kudurru object with a plain JSON Schema validator
// (github.com/santhosh-tekuri/jsonschema/v6) plus cel-go used directly for
// the x-kubernetes-validations rules, without k8s.io/apiextensions-apiserver.
// Deliberately crude: no defaulting, ratcheting, list-type, metadata or
// apiserver-identical messages; it exists to size the fallback.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func init() { spikeCmds = append(spikeCmds, newSpikeFallbackCmd) }

type celRule struct {
	path []string // property path; "[]" descends into list items
	rule string
	msg  string
	prg  cel.Program
}

func collectRules(s map[string]any, path []string, out *[]celRule) {
	if xs, ok := s["x-kubernetes-validations"].([]any); ok {
		for _, x := range xs {
			m := x.(map[string]any)
			msg, _ := m["message"].(string)
			*out = append(*out, celRule{path: append([]string(nil), path...), rule: m["rule"].(string), msg: msg})
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, v := range props {
			collectRules(v.(map[string]any), append(path, k), out)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		collectRules(items, append(path, "[]"), out)
	}
}

func nodes(v any, path []string, at string, f func(string, any)) {
	if len(path) == 0 {
		f(at, v)
		return
	}
	if path[0] == "[]" {
		l, _ := v.([]any)
		for i, it := range l {
			nodes(it, path[1:], fmt.Sprintf("%s[%d]", at, i), f)
		}
		return
	}
	m, _ := v.(map[string]any)
	if c, ok := m[path[0]]; ok {
		nodes(c, path[1:], strings.TrimPrefix(at+"."+path[0], "."), f)
	}
}

func newSpikeFallbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "spike-fallback CRD OBJECT",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			crdDoc, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			objDoc, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			var crd map[string]any
			if err := yaml.Unmarshal(crdDoc, &crd); err != nil {
				return err
			}
			schema := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
			sj, _ := json.Marshal(schema)
			sdoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(sj))
			if err != nil {
				return err
			}
			comp := jsonschema.NewCompiler()
			if err := comp.AddResource("crd.json", sdoc); err != nil {
				return err
			}
			sch, err := comp.Compile("crd.json")
			if err != nil {
				return err
			}
			var rules []celRule
			collectRules(schema, nil, &rules)
			env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
			if err != nil {
				return err
			}
			for i := range rules {
				ast, iss := env.Compile(rules[i].rule)
				if iss.Err() != nil {
					return iss.Err()
				}
				if rules[i].prg, err = env.Program(ast); err != nil {
					return err
				}
			}
			oj, err := yaml.YAMLToJSON(objDoc)
			if err != nil {
				return err
			}
			odoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(oj))
			if err != nil {
				return err
			}
			if err := sch.Validate(odoc); err != nil {
				fmt.Println("schema:", err)
			}
			var obj any
			_ = json.Unmarshal(oj, &obj)
			for _, r := range rules {
				if strings.Contains(r.rule, "oldSelf") {
					continue // transition rules need the old object
				}
				nodes(obj, r.path, "", func(at string, v any) {
					out, _, err := r.prg.Eval(map[string]any{"self": v})
					if err != nil || out.Value() != true {
						fmt.Printf("cel: %s: %s\n", at, r.msg)
					}
				})
			}
			fmt.Println("done")
			return nil
		},
	}
}
```

### `omitzero/main.go`

Probe: `omitzero` vs `omitempty` for metav1.Duration.

```go
// Probe: does `omitzero` drop a zero metav1.Duration in the encoders a
// Kubernetes client uses?
package main

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type withOmitEmpty struct {
	TTL metav1.Duration `json:"ttl,omitempty"`
}

type withOmitZero struct {
	TTL metav1.Duration `json:"ttl,omitzero"`
}

func main() {
	a, _ := json.Marshal(withOmitEmpty{})
	b, _ := json.Marshal(withOmitZero{})
	ua, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&withOmitEmpty{})
	ub, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&withOmitZero{})
	fmt.Printf("encoding/json omitempty: %s\nencoding/json omitzero:  %s\n", a, b)
	fmt.Printf("unstructured omitempty: %v\nunstructured omitzero:  %v\n", ua, ub)
}
```

### `setup-kubefb.sh`

Kube variant with the fallback prototype instead of the validator (single-binary comparison).

```sh
#!/usr/bin/env bash
# setup-kubefb.sh — src-kubefb: the kube variant plus the fallback prototype
# instead of the validator, then a release build and its size.
set -euo pipefail
T=/home/vercel-sandbox/kudurru/.claude/worktrees/wf_cd87f6f4-670-4/.spike-tools
P=$T/patches
export PATH=/usr/local/go/bin:$PATH CGO_ENABLED=0
rm -rf "$T/src-kubefb"
cp -r "$T/src-kube" "$T/src-kubefb"
cp "$P/spike_fallback.go" "$T/src-kubefb/cmd/kudurru/"
cd "$T/src-kubefb"
go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3 >/dev/null 2>&1
go mod tidy >/dev/null 2>&1
mkdir -p "$T/out2/kubefb"
go build -trimpath -ldflags "-s -w" -o "$T/out2/kubefb/kudurru" ./cmd/kudurru
go version -m "$T/out2/kubefb/kudurru" > "$T/out2/kubefb/deps.txt"
echo "kubefb size=$(stat -c %s "$T/out2/kubefb/kudurru") deps=$(grep -c $'^\tdep\t' "$T/out2/kubefb/deps.txt")"
```
