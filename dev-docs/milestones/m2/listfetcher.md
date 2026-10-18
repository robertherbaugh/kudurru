# M2 work package: listfetcher (leader-fetched blocklists, internal listener, starter list)

Branch `worktree-wf_ae7e11e2-8a2-3`, based on `e56cbea`. Covers
dev-docs/design-record.md §3.1 (listfetcher and internal rows), §4.5, §4.6
(Blocklists), §6.4 (starter list licensing), §7.3 ("Fetch once, serve
everywhere" and the list pipeline), decisions D15, D47 and D73.

Packages: `internal/listfetcher` (new), `internal/cluster/internalapi`
(new), `internal/cluster/membership.go` (new file), `internal/listdata`
(new), additive changes to `internal/blocking` (exported `SourceName`),
`internal/bootstrap` (`EmbeddedStarterBlockList`) and `internal/config`
(blocking and internal-listener keys). `cmd/` is untouched; the wiring is
below.

## What was built

### `internal/listfetcher`

Two halves that share one `DiskCache`:

- **`DiskCache`** (`blocking.listsDir`, default `<dataDir>/lists`). One body per
  source, named `sha256(key)`, where the key is the list URL or
  `configmap://<ns>/<name>`. A JSON sidecar `<name>.meta` holds the digest, size,
  ETag, Last-Modified, fetched/changed times, origin (`fetch`, `leader`, `self`)
  and the parser counts.
  - `Put` streams into a temporary file while hashing. It enforces the size cap
    and an optional expected digest, runs the validator, fsyncs, and then renames
    atomically. An identical body only refreshes the metadata. Any error leaves
    the previous body in place.
  - `OpenDiskCache` deletes crash leftovers and orphans, and re-hashes every body
    against its sidecar, dropping entries that do not match.
  - `OpenList(digest)` implements `internalapi.ListOpener`.
- **`Downloader`**: conditional GET (`If-None-Match`, `If-Modified-Since`; a 304
  only touches the metadata), 3 attempts on network errors, 429 and 5xx, with
  jittered exponential backoff (1 s, then 2 s, ±50 %). Other 4xx responses and
  content errors are not retried.
  - Bodies over `maxHTTPSize` (64 MiB) are refused. So are HTML and binary
    bodies (`parser.ErrHTML` and `parser.ErrBinary` from `parser.Parse`), empty
    lists, and bodies whose invalid lines exceed `maxErrorsPerSource` (5) while
    also outnumbering their rules. See Surprises for why this differs from blocky.
  - Only http and https URLs are accepted. Credentials and query strings are
    redacted from logs and status.
  - `configMapRef` sources go through a `ConfigMapFunc` seam. Without one they
    report `ErrUnsupported`.
- **`Fetcher`**, the leader-only runnable (`NeedLeaderElection() == true`). On
  every BlockList change, and whenever the next source falls due, it:
  1. Plans every source. A URL shared by several lists is downloaded once, on the
     shortest refresh interval of those lists.
  2. Downloads due sources with bounded concurrency (4, `errgroup`). A source is
     due one refresh interval after its last success, minus a deterministic
     per-URL jitter of up to 5 %. A cooldown applies after failures: 1 min,
     doubling up to 1 h.
  3. Publishes the owned status fields of each list, as described in "Status
     ownership" below.
  - Refresh timers resume from `status.sources[].lastFetched` (§4.6), so a new
    leader does not refetch everything.
  - A new leader does not recompile either: counts are reused from status while
    the digests match.
- **`Resolver`**, the always-on runnable on every node. For each BlockList it
  resolves every source in this order:
  1. the local cache by the published digest;
  2. else `GET /internal/lists/{digest}` from the leader (`cluster.Membership`),
     with the sha256 verified while streaming and committed only on a match;
  3. else, after `FallbackAfter` (10 min) without a reachable leader or with the
     source still pending, its own download.

  While a newer body is pending, the node keeps serving the older one it has.
  - It compiles with `blocking.Rebuild`, which reuses unchanged lists by CRC, and
    calls `SetLists` only when the inputs changed (a fingerprint of every list,
    mode, source, format and digest).
  - Run starts with a cache-only pass, so a restarted node serves its last lists
    before touching the network.
  - Cache entries that no BlockList references are removed, but only while
    zonecache is `Live`.
  - On the leader it never pulls from itself and never self-downloads: the
    fetcher owns downloads there.
  - Init strategies:
    - `fast`: ready at once; lists load in the background.
    - `blocking`: not ready until every source has a body or has failed
      definitively, unless `allowDegraded`.
    - `failOnError`: as `blocking`, and `Run` returns `ErrInitialLoad` when the
      initial load had a definitive failure. As an always-on runnable this stops
      the process.
- **`ZoneCacheObjects`** adapts `zonecache.Cache` to the `Objects` interface
  (BlockLists, change notifications through `OnPublish` filtered on
  `Changed(BlockList)`, `Initialized`, `Live`). Create one per Cache and share it
  between the fetcher and the resolver.
- **`ListName(ns, name) = "ns/name"`** is the compiled list name in the blocking
  snapshot, the query log and metrics.

### Status ownership (closes the follow-up)

The fetcher writes only these fields:

- `status.sources[].{name,digest,etag,entries,invalid,lastFetched,error}`
- `status.entries`, `status.ruleCount`, `status.invalid`
- `status.lastFetched`

How the write works:

- It never touches `observedGeneration` or the conditions. The controller keeps
  deriving `Degraded{ListFetchFailed}` from `sources[].error` (unchanged code in
  `internal/controller/refs.go`).
- Each write re-reads the object, replaces only those fields, and calls
  `UpdateStatus` with the object's resourceVersion. On a conflict it retries, up
  to 5 times. The sqlite and kube drivers check the resourceVersion; the memory
  driver does not, which is fine for tests.
- It skips the write when the spec moved on since it planned the status (a
  different generation or different sources), so an old plan cannot overwrite
  status for a new spec.
- Writes happen only when the owned fields differ. With metav1 second precision,
  that is once per change, plus once per refresh for `lastFetched`.
- A source not yet attempted in the current term keeps its previous `error`, so
  `Degraded` does not flap on a leader change.

Tests count `UpdateStatus` calls through a wrapping store. They check: one
write despite two injected conflicts, zero writes over ten idle passes and
after a controller-only change, and zero writes and zero refetches on a new
term.

### `internal/cluster/internalapi` (D47)

- **Credentials** (`NewCredentials(CredentialOptions)`):
  - Token: `base64url(HKDF-SHA256(key, info "kudurru/internal/v1", 32))`.
  - CA: an Ed25519 key from `HKDF-SHA256(key, info "kudurru/internal-ca/v1",
    32)` and a deterministic self-signed certificate. The CA is byte-identical on
    every node.
  - Each node issues itself a 90-day node certificate with an ephemeral Ed25519
    key, renewed 30 days before expiry. Its DNS SAN is
    `internal.kudurru.invalid` plus the node name. Clients verify that name
    against the derived CA.
  - TLS 1.3 only.
  - Overrides: `TokenFile`, `CertFile`/`KeyFile`, `CAFile`, `ServerName`.
  - Rotation (`PreviousKey`): both tokens are accepted and both CAs trusted. Node
    certificates are issued by the previous CA, which every node trusts, and
    clients retry with the previous token on 401. Old and new nodes therefore
    talk in both directions while a rollout is in progress (tested).
- **Server** (`NewServer(ServerOptions)`, `Run(ctx)`, `Serve(ctx, ln)`,
  `Handler()`):
  - `GET /internal/status` returns `{node, version, leader, state, lists:[{name,
    digests}]}`.
  - `GET /internal/lists/{digest}` streams the cached body with `Content-Length`,
    `ETag`, `X-Kudurru-Digest` and `X-Kudurru-Source`.
  - Every request goes through a per-address token bucket (20/s, burst 40; 429
    with `Retry-After`), a no-request-body rule (400) and a constant-time bearer
    token check (401).
  - Limits: at most 4 concurrent list downloads (503 with `Retry-After`), a
    256 MiB body cap (413), and 16 KiB of headers.
  - Timeouts: 5 s to read headers, 10 s to read the request, a 10 s write
    deadline for status, and for lists a write deadline of 2 min plus 1 s per MiB.
  - Metric: `kudurru_internal_requests_total{route,code}`.
- **Client** (`NewClient(creds, ClientOptions)`, `Status`, `OpenList`):
  `OpenList` returns a reader that verifies the sha256. It returns
  `ErrDigestMismatch` instead of `io.EOF` on a mismatch, and `ErrTooLarge` past
  the cap, so a caller that commits only after EOF never keeps a bad body. A 404
  is `ErrListNotFound`.

### `internal/cluster` membership

`Membership` interface (`Leader(ctx) (Peer, error)`, `Peers(ctx)`), with three
implementations:

- `Self{Name}`: Docker single-instance and ha-static, where every node leads
  itself.
- `Static{SelfName, LeaderName, Members}`: a fixed list.
- `ElectorMembership{SelfName, Elector, Resolve}`: the Lease holder's identity,
  resolved through a hook and cached per leader.

`ErrNoLeader`.

### Starter list (§6.4)

- `internal/listdata` embeds Steven Black's own ad-hoc list
  (`data/StevenBlack/hosts` at `2025dea10a1d`, MIT) with go:embed. It ships with
  the MIT `LICENSE` and a `PROVENANCE.md` giving the SPDX id, the upstream commit,
  the SHA-256 and what is done to the data.
- This is the same file the blocking spike vetted as test data. The project's
  unified hosts file aggregates CC BY-SA and non-commercial sources, so it is
  never embedded; nothing from hagezi or AdGuard is embedded either.
- `listdata.StarterBlockList` returns BlockList `starter`: block mode, one inline
  `domains` source `stevenblack-adhoc` with 2,852 deduplicated host names (about
  74 KB), and a `kudurru.io/provenance` annotation.
- `bootstrap.EmbeddedStarterBlockList` is the `StarterBlockListFunc` that wires
  it in. Seed creates it only on a genuine first boot and adds it to
  `Policy/default`. A deleted starter list stays deleted (tested through the CRD
  validator).
- `TestProvenance` is the licences.yml-style check. The provenance must carry an
  SPDX id from a permissive allow-list (MIT, BSD, Apache-2.0, ISC, 0BSD, CC0,
  Unlicense) and name it in the table. LICENSE must be the licence text with a
  copyright line, and the recorded SHA-256 must match the embedded bytes.

### Config keys (additive; drift test green)

| Key | Default | Scope / flags |
|---|---|---|
| `blocking.refreshInterval` | `24h` (minimum 1m) | cluster |
| `blocking.initStrategy` | `fast` (`fast`, `blocking`, `failOnError`) | cluster, restartRequired |
| `blocking.allowDegraded` | `true` | cluster |
| `blocking.listsDir` | `""` = `<node.dataDir>/lists` (`cfg.BlockingListsDir()`) | node, hostOnly |
| `listeners.internal.address` | `0.0.0.0:8082` | node, hostOnly |
| `listeners.internal.tokenFile` | `""` (derive from the key) | node, hostOnly |
| `listeners.internal.tls.{certFile,keyFile,caFile,serverName}` | `""` (derive) | node, hostOnly |

Validation rejects an unknown initStrategy, a refreshInterval under 1m, and
certFile without keyFile.

## Wiring for `cmd/kudurru serve`

```go
// After zonecache is constructed (before runner.Run).
lists, err := listfetcher.OpenDiskCache(cfg.BlockingListsDir(), logger)
if err != nil { return err }
objs := listfetcher.NewZoneCacheObjects(zc)          // one per zonecache
dl := listfetcher.NewDownloader(lists, listfetcher.DownloadOptions{
    // ConfigMaps: kube driver only (seam; nil -> configMapRef reports Unsupported)
})
strategy, err := listfetcher.ParseInitStrategy(cfg.Blocking.InitStrategy)
if err != nil { return err }

// Leader-only: downloads and status.
fetcher, err := listfetcher.New(listfetcher.Options{
    Store: st, Objects: objs, Cache: lists, Downloader: dl,
    RefreshInterval: cfg.Blocking.RefreshInterval.Std(),
    Registerer: reg, Logger: logger,
})

// Membership: Docker (sqlite) -> cluster.Self; kube -> Lease holder.
var membership cluster.Membership = cluster.Self{Name: cfg.Node.Name}
if cfg.Store.Driver == "kube" {
    membership = &cluster.ElectorMembership{
        SelfName: cfg.Node.Name,
        Elector:  st.Elector(cluster.ElectionController),   // same instance the Runner uses
        Resolve: func(_ context.Context, pod string) (string, error) {
            return net.JoinHostPort(pod+"."+headlessService, "8082"), nil   // or an EndpointSlice lookup
        },
    }
}

// Key bytes: KUDURRU_ENCRYPTION_KEY or KUDURRU_ENCRYPTION_KEY_FILE (and
// _PREVIOUS[_FILE]), decoded with session.ParseKey (32 bytes).
key, prevKey := readEncryptionKeys()                      // nil when unset
var client *internalapi.Client
var internalSrv *internalapi.Server
var resolver *listfetcher.Resolver                        // assigned below; read by the status closure
if key != nil || cfg.Listeners.Internal.TokenFile != "" {
    creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{
        Key: key, PreviousKey: prevKey, NodeName: cfg.Node.Name,
        TokenFile: cfg.Listeners.Internal.TokenFile,
        CertFile: cfg.Listeners.Internal.TLS.CertFile, KeyFile: cfg.Listeners.Internal.TLS.KeyFile,
        CAFile: cfg.Listeners.Internal.TLS.CAFile, ServerName: cfg.Listeners.Internal.TLS.ServerName,
    })
    if err != nil { return err }
    client = internalapi.NewClient(creds, internalapi.ClientOptions{})
    internalSrv, err = internalapi.NewServer(internalapi.ServerOptions{
        Address: cfg.Listeners.Internal.Address, Credentials: creds, Lists: lists,
        Status: func(ctx context.Context) internalapi.Status {
            l, _ := membership.Leader(ctx)
            return internalapi.Status{Node: cfg.Node.Name, Version: info.Version,
                Leader: l.Name, State: zc.State().String(), Lists: resolver.Lists()}
        },
        Registerer: reg, Logger: logger,
    })
    if err != nil { return err }
} else if cfg.Store.Driver == "kube" {
    return errors.New("KUDURRU_ENCRYPTION_KEY is required for the internal listener in Kubernetes")
}

// Every node: bodies by digest, compile, SetLists.
resolver, err = listfetcher.NewResolver(listfetcher.ResolverOptions{
    Objects: objs, Cache: lists, Sink: zc, Membership: membership, Client: client,
    Downloader: dl, RefreshInterval: cfg.Blocking.RefreshInterval.Std(),
    InitStrategy: strategy, AllowDegraded: cfg.Blocking.AllowDegraded,
    Registerer: reg, Logger: logger,
})
checker.Register("blocklists", resolver.Ready)

// Runnables
{"listfetcher", true,  fetcher.Run},     // leader-only
{"listresolver", false, resolver.Run},   // always-on; returns ErrInitialLoad under failOnError
{"internal", false, internalSrv.Run},    // always-on, only when internalSrv != nil

// Bootstrap: the embedded starter list on first boot.
bootstrap.Seed(ctx, st, bootstrap.Options{Validator: v, Logger: logger,
    StarterBlockList: bootstrap.EmbeddedStarterBlockList})
```

Notes for the integrator:

- With `blocking` or `failOnError`, `SetStarted` may also wait on
  `<-resolver.Loaded()`. `fast` needs nothing.
- In Docker single-instance mode no key is needed. Membership is `Self`, the
  resolver reads what the fetcher writes into the same cache, and the internal
  listener is optional. Expose `:8082` only when a key or token file is set.
- The policy stage (another package) must look lists up as
  `listfetcher.ListName(policy.Namespace, ref)`, i.e. `"ns/name"`.
- Metric names are new and unique, so registration does not collide with
  anything in M1.

## Measurements

Shared 8-CPU sandbox (other agents running), Go 1.27.2, no `-race` unless
noted.

| What | Result |
|---|---|
| Replica pull of a 1M-entry list (31.5 MiB) over the internal listener into its cache: TLS 1.3, bearer token, streaming sha256, fsync, rename (`BenchmarkPullByDigest`) | 90-237 ms per pull (139-367 MB/s), 1.5 MB allocated |
| Leader: changed 1M-entry list, conditional GET, parser validation, compile for the counts, one status write (`BenchmarkFetchAndPublish1M`) | 0.70 s, 231 MB allocated (one list in flight) |
| Resolver pass over 100 unchanged lists, which is the cost of every BlockList status event on a node (`BenchmarkResolvePass`) | 0.26 ms, 245 KB, no rebuild |
| End to end, two in-process nodes on zonecache: Apply BlockList → leader fetch → status → replica pulls by digest → compiled in the replica's zonecache (`TestEndToEndTwoNodes`, `-race`) | 167 ms, including the zonecache debounces; the list host was hit once |
| Status writes per list change | 1 (plus conflict retries); 0 on idle passes and on a new leader term |
| Starter list | 2,852 domains, about 74 KB inline in one BlockList, compiled entries equal to the original hosts file |
| Binary (CGO_ENABLED=0, `-s -w`), serve not yet wired | 46.6 MB; the embedded data adds about 100 KB; listfetcher and internalapi link in when serve wires them |
| `go test -race ./internal/listfetcher/` | about 1.6 s |

## Tests

- **listfetcher**:
  - Fetch pipeline against httptest, run as one sequence:
    1. first fetch with digest, ETag, counts and gauges;
    2. an idle pass makes no request and no write;
    3. a 304 revalidation moves only `lastFetched`;
    4. a 500 is retried 3 times, then keep-old plus an error plus the failure
       and stale metrics, then cooldown;
    5. an HTML body is rejected without a retry;
    6. the size cap;
    7. recovery with a new digest;
    8. deleting the list drops its metrics.
  - Status ownership and UpdateStatus counting.
  - configMapRef reports Unsupported and an ftp URL is rejected.
  - A shared URL is fetched once.
  - Digest pull between two in-process nodes over TLS, including following v2.
  - Fallback timing with a fake clock: no download at 9 min 59 s, one at
    10 min, none after.
  - No leader and no digest falls back after the delay.
  - The init strategy matrix (fast, blocking, blocking+allowDegraded,
    failOnError), plus failOnError and blocking with a definitive failure.
  - A disk-seeded offline pass and GC of unreferenced cache entries.
  - `SetLists` integration with a real zonecache on the memory driver: add, edit
    and delete.
  - Docker shape: the leader's resolver never downloads on its own.
  - End-to-end runnables on two nodes.
  - DiskCache: put, identical put, failures keep the old body, reopen with
    corruption, crash leftovers and orphans, Remove.
  - Downloader validation table, retries with backoff bounds, 404 not retried,
    URL redaction.
- **internalapi**: derivation determinism and key separation; two nodes for
  status and list pull; digest mismatch; client and server size caps; foreign key
  (unknown authority); wrong token (401); handler-level checks for a missing
  token, a request body, a bad digest and POST; key rotation in both directions
  and after completion; rate limit; download slots; file overrides with an
  operator CA; node certificate renewal.
- **cluster**: Self, Static (table) and ElectorMembership (caching, resolve
  errors, own identity without a term).
- **listdata**: provenance and licence check; the starter list compiles to the
  original's entries with exact (not subdomain) matching.
- **bootstrap**: the embedded starter list through the CRD validator, never
  recreated after deletion.
- **config**: defaults, file plus env overrides, validation.

## Surprises

1. **blocky's `maxErrorsPerSource` (default 5) would reject browser filter
   lists.** kudurru's parser counts cosmetic rules (`##.banner`) as invalid
   lines, and EasyList-style lists contain thousands of them while §7.3 promises
   such lists are "safe". The check therefore refuses a body only when its
   invalid lines exceed the limit **and** outnumber its rules, which catches
   error pages and wrong files. `-1` disables the check.
2. **Key rotation needs the previous CA as issuer.** A node can present only one
   certificate. Signing with the new key would break old-key nodes dialling new
   ones. While `PreviousKey` is set, node certificates come from the previous CA,
   which old nodes trust as their current CA and new nodes as their previous CA.
3. **`SourceStatus` has no URL or Last-Modified field.** Last-Modified lives in
   the cache sidecar, so a new leader's first refresh is an unconditional GET
   when only the ETag is known. Replicas record the published ETag on a pull, so
   a promoted replica can still revalidate conditionally.
4. **The resolver on the leader must never self-download.** The fetcher owns
   downloads there, and with no digest yet published the fallback timer would
   otherwise fire on the leader too. The resolver asks the membership before
   falling back.
5. Inline sources get a digest too (sha256 of the lines joined by `\n`), so
   `/internal/status` reports every source uniformly.

## Follow-ups

- **Serve wiring** as above (integrator). Also: read
  `KUDURRU_ENCRYPTION_KEY[_PREVIOUS][_FILE]` with `session.ParseKey`, choose the
  kube resolve hook (headless Service DNS for StatefulSets, an EndpointSlice
  lookup for Deployments), and add `8082` to the chart's Service and
  NetworkPolicy.
- **Static peers config key**: `cluster.peers` / `cluster.primary` (v0.2
  follower) to build `cluster.Static` from configuration. The `cluster.*` section
  belongs to the `cluster.role` follow-up, so this package does not add it.
- **ConfigMap source**: implement `ConfigMapFunc` with the kube driver's client
  (needs `get configmaps` in the release namespace).
- **api/v1alpha1**: a `lastModified` field in `SourceStatus` would let a new
  leader revalidate with `If-Modified-Since`. `BlockListStatus.unsupported`
  (already a follow-up) could be filled from `Stats.Unsupported`.
- **Download options as config keys** if operators need them:
  `blocking.maxHTTPSize`, `blocking.concurrency`, `blocking.maxErrorsPerSource`.
  Today they are Options with blocky's defaults.
- **Nodes page**: scatter-gather `/internal/status` over `Membership.Peers` (an
  EndpointSlice source in kube).
- **`/internal/events`** (NOTIFY fan-out) and **`/internal/snapshot`** (v0.2)
  can join the same Server mux and guard.

## Recommended changes to dev-docs/design-record.md

- §3.1 internal row and D47: document the HKDF info strings, the fixed
  verification name `internal.kudurru.invalid`, TLS 1.3 only, and the rotation
  rule (issue from the previous CA while `_PREVIOUS` is set; clients retry the
  previous token on 401).
- §7.3: replace "maxErrorsPerSource" with the refined rule from Surprise 1. State
  that replicas keep serving their older cached body while a newer published
  digest is pending, and that the 10-minute fallback timer also starts when no
  digest has been published.
- §6.2: add `blocking.listsDir` and the `listeners.internal.tls.*` keys to the
  example. `tls: { auto: derived }` became four optional file keys plus
  `serverName`.
- §6.4: name the embedded source precisely: "StevenBlack/hosts
  `data/StevenBlack/hosts` (Steven Black's own MIT list), never the unified
  file".
