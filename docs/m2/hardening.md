# M2 work package: hardening (M1 carry-overs and driver hardening)

Branch `worktree-wf_ae7e11e2-8a2-4`, based on `e56cbea`. Covers the M1 exit criteria carried over in docs/followups.md (multi-socket listeners, cache persistence, the §7.1 step 2 QPM limiter, store and upstream metrics), the store-driver follow-ups (memory watch delivery, kube `LastContact`, removal of the kube snapshot helpers, the cold-start cluster check) and the build items (D75 size budget, envtest cache, NOTICE).

Commits: `71c77e6` multi-socket listeners, `0237c3b` rate limiter, `5db8949` cache persistence, `742d1fa` store hardening, `31df7d3` metrics, `3e11c48` build/CI/NOTICE, `81c07fd` catch-up counter rename, `bad0d41` limiter end-to-end test, plus the docs commit with this report. They close the followups.md rows for: multi-socket listeners, cache Dump/Load, ingress QPM limiting, `kudurru_store_*` metrics, upstream responses/inflight/proto, memory driver watch delivery, kube `LastContact`, kube `PersistSnapshots`/`MaterializedSnapshot`, the cold-start cluster ID, the CI size budget, envtest caching and the NOTICE headings.

## What was built

### 1. Multi-socket SO_REUSEPORT listeners (`internal/dnsserver`)

- `socket.go`, `socket_linux.go`, `socket_other.go`. Each `udp` and `tcp` Spec binds N sockets on one address with `SO_REUSEPORT`, set through `net.ListenConfig.Control` with `golang.org/x/sys/unix`. Each socket has its own miekg `dns.Server` and read loop, and all of them share the listener's `Handler`. DoT stays at one socket. Platforms other than Linux always bind one.
- `Options.Sockets` (`listeners.sockets`): 0 means `runtime.GOMAXPROCS(0)` and 1 means a single plain socket. `SocketCount(n)` resolves the value, and `ReuseportSupported` reports the platform.
- **Port conflicts still fail fast.** With SO_REUSEPORT, a second process running as the same user could otherwise join a port that is already serving, and the kernel would split traffic between the two. So the first bind is a plain probe without the option. If the port is taken, it fails with the same error as before (`TestRegistryBindIsAllOrNothing` still passes unchanged, and `TestMultiSocketBindConflict` covers the reuseport case). The probe is then closed and the group binds on the probe's port, which also resolves port 0. The configured host is kept, so `0.0.0.0:53` stays IPv4-only.
- `dns.go` changes: a listener holds `[]*dns.Server`; it is Ready once every socket's server has started. On shutdown every server drains in parallel. On a socket error the others are stopped and the first error is returned. `socketServer` copies every exported setting of the template server (TsigProvider, TsigSecret, MsgAcceptFunc, Decorate*, ...), so the TSIG agent's settings on the template reach every socket.
- **Drop signal.** A stage can now ask the UDP transport to send nothing. It answers with `Response{Msg: nil, Status: StatusDropped | StatusBlockedRateLimit}`, which `dnsserver.IsDrop` recognises. `serveMiekg` writes nothing over UDP and SERVFAIL over TCP. `Handler.Resolve` keeps its never-nil contract and turns a drop into SERVFAIL for DoT, DoH, DoQ and DoH3. This needed no change to `internal/pipeline`.

### 2. Ingress rate limiting (`internal/stages/ingress/ratelimit.go`)

- Token bucket per prefix: IPv4 /32 = 600 QPM and /24 = 6000; IPv6 /128 = 600, /64 = 6000 and /56 = 12000 (all configurable). A bucket holds one minute's allowance (burst = QPM) and refills at QPM/60 per second. Levels are charged most specific first, and a limited /32 stops before it can drain its /24.
- Over the limit, `Slip` percent (default 50) get an empty `TC=1` NOERROR reply, so a real client retries over TCP. The rest are dropped (nil message, see the drop signal above). The slip ratio is spread evenly over the sequence of over-limit queries (Bresenham), with no randomness.
- Only `pipeline.ProtoUDP` is limited. TCP, DoT, DoH, DoQ and DoH3 are not, because a handshake proves the source address. The charged client is `req.Client.Addr`, the PROXYv2-attributed address. Loopback, `ratelimit.exempt` prefixes and `health.kudurru.internal.` are never charged. IPv4-mapped IPv6 addresses use the IPv4 levels.
- Buckets live in an LRU of 64 shards bounded by `MaxBuckets` (default 65536). An evicted prefix starts with a full bucket.
- Hooks follow the proxyproto pattern. `SetHook(func(Verdict))` is called synchronously for each slipped or dropped query, and `Collector()` returns the `kudurru_ratelimit_buckets` gauge. `DropReason = "ratelimit"`.
- The limiter runs first in `Stage.Serve`, before the FORMERR/NOTIMP/BADVERS checks. Invalid floods are therefore charged too, a deliberate change from the §7.1 listing order. The health name stays exempt and is still answered locally while the client is limited.
- Config: `RateLimitFromConfig(config.RateLimit) (*RateLimitConfig, error)` returns nil when `ratelimit.enabled` is false.

### 3. Cache persistence (`internal/store/localdb`, `internal/cache`, `internal/cache/persist`)

- **localdb.** New `blobs(name TEXT PRIMARY KEY, data BLOB, written_at_ns INTEGER)` table, behind the `BlobStore` interface (`PutBlob`, `GetBlob`) plus `DeleteBlob` and `ErrNoBlob`. `*SnapshotStore` implements it.
- **Schema migration.** local.db's table layout now has its own version, `meta.layout_version` (`LayoutVersion = 2`), with steps `[schemaV1, schemaV2Blobs]`. `meta.schema_version` stays `store.SchemaVersion`, which versions the stored API objects. A newer layout refuses to open with `ErrSchemaTooNew`. A database written before layout versions existed migrates in place (`TestLayoutMigration`).
- **cache.** `Dump` now delegates to the new `WriteDump(w) (int, error)`. The package also defines the persistence hook contract: `Persister{Save, Restore}`, returning `PersistStats{Entries, Bytes, WrittenAt}`, and `RunPersister(ctx, c, p, interval, finalTimeout, logger)`. That runnable saves every `interval` (never when 0) and once more when ctx ends.
- **cache/persist.** `New(localdb.BlobStore, Options{Name, MaxAge, NoCompress, Now})` implements `cache.Persister`. The blob layout is `"KDRP"` | version | flags (bit 0 = gzip) | written-at in Unix ns | `cache.Dump` payload, gzip `BestSpeed` by default.
  - `Restore` returns zero stats when there is no blob, `ErrStale` past `MaxAge` (default 24 h), `ErrIncompatible` for another format, and `cache.ErrBadDump` for a damaged payload.
  - Entries keep their absolute expiry, so entries that expired during the downtime are skipped by `cache.Load`.

### 4. Driver hardening

- **memory.** Initial Watch events now come from a goroutine for any number of objects. The list is read and the watcher registered under one lock. A watcher with more than `WatchBuffer` (256) queued live events is unregistered and its channel is closed, so the consumer re-lists. Nothing is dropped silently any more. Live events now honour `WatchOptions.Namespace`, which used to apply only to the initial list. `Stop` is idempotent and returns after the channel closes.
- **conformance.** New case `SlowWatcherGetsClosedOrResynced` in `slowwatcher.go`. It writes `SlowWatcherWrites` (1500) objects from 8 writers while the watcher is not read. Then either every object arrives on the open stream, or the channel closes and a new Watch lists every object before its Bookmark. Results: memory closes; sqlite catches up from its change feed (1501 objects); kube's informers buffer and catch up (1.5 s on envtest).
- **store.Info.LastContact** (`time.Time`, additive). This is the one edit to `internal/store/store.go`. Zero means never, or a local driver.
  - The kube driver wraps its rest.Config transport (`contact.go`). Any response below 500 counts as contact, and so does every successful read of a response body, which keeps a long-running watch fresh. 5xx and transport errors do not count.
  - With the Lease elector polling every 2 s, a healthy replica's value is a few seconds old.
- **kube.** `MaterializedSnapshot`, `PersistSnapshots`, `persistOnce` and `ErrNotSynced` are removed (D79: zonecache owns local.db). Their tests were adapted, not dropped:
  - The closed-store check now covers `Watch`.
  - The RBAC test checks a Watch reaching its Bookmark and a non-zero `LastContact`.
  - The measurement uses a paged List.
  - `TestColdStartFromLocalDB` keeps its outage and local.db assertions and adds the LastContact and cluster-ID ones.
  - New `TestLastContactDuringPartition`: contact freezes during a 3 s partition and comes back 1.0 s after healing.
- **zonecache.** The cold-start cluster check is a separate function in `coldstart_cluster.go`, called from `startupFallback` (a 5-line insertion in persist.go). A snapshot whose `ClusterID` differs from `store.Info().ClusterID` is refused: not ready, with "local.db snapshot rejected: ...", until the store syncs. If either ID is unknown, the snapshot is served unverified.

### 5. Metrics

- **sqlite** (`Options.Metrics`, created with `sqlite.NewMetrics(reg)`):
  - `kudurru_store_operations_total{op,result}`. op is one of get, list, apply, delete, update_status, watch, snapshot, restore. result is one of ok, not_found, conflict, managed_by, invalid, closed, canceled, error.
  - `kudurru_store_operation_duration_seconds{op}`. Writes include the commit, so this is the write-latency signal.
  - `kudurru_store_watch_catchups_total{kind}`: watchers that fell behind and caught up from the feed. This is the feed-lag signal; see Surprises for why it is not `..._resyncs_total`.
  - `kudurru_store_watchers`.
- **upstream.**
  - `kudurru_upstream_responses_total{upstream,rcode}`: every response received. The four common rcodes are pre-resolved per server.
  - `kudurru_upstream_inflight{upstream}`.
  - A `proto` label on `kudurru_upstream_requests_total`: udp, tcp, dot, doh, doh3, doq or dnscrypt. It comes from the configured scheme (dnsproxy rewrites `h3://` to `https://`). An `sdns://` stamp is labelled by the upstream dnsproxy built from it.
  - Reloads delete every series of removed servers (`DeletePartialMatch`).

### 6. Build and CI

- NOTICE: client-go, controller-runtime and modernc.org/sqlite move from "Test data" to the linked dependencies. golang.org/x/sys is added, along with the CoreDNS multisocket design credit.
- `make size` builds the stripped `CGO_ENABLED=0` binary, prints its size, budget, percentage used and headroom, and fails above `SIZE_BUDGET = 51,234,000` bytes. That is 10 % above the M1 binary of 46,575,776 bytes (linux/amd64, Go 1.27.2).
- `ci.yml`:
  - `actions/cache@v4` on `bin/k8s`, keyed on the Makefile (which pins `ENVTEST_K8S_VERSION` and setup-envtest). `make envtest` is skipped on a cache hit.
  - A "Binary-size budget (D75)" step runs `make size` after the build.

## Serve wiring (for the integrator; nothing in cmd/ was touched)

```go
// SQLite store metrics (openStore, sqlite branch):
st, err := sqlite.Open(path, sqlite.Options{ /* as today */ Metrics: sqlite.NewMetrics(reg)})

// local.db for every driver: blobs need it under sqlite too. Keep passing
// it to zonecache as LocalDB for kube as today (or for both, per D79).
localDB, err := localdb.OpenSnapshotStore(ctx, filepath.Join(cfg.Node.DataDir, localdb.FileName))

// Ingress limiter.
var limiter *ingress.RateLimiter
rlc, err := ingress.RateLimitFromConfig(cfg.RateLimit)
if err != nil { return err }
if rlc != nil {
	if limiter, err = ingress.NewRateLimiter(*rlc); err != nil { return err }
	limiter.SetHook(func(v ingress.Verdict) {
		if v == ingress.VerdictDrop {
			dnsMetrics.Dropped.WithLabelValues(ingress.DropReason).Inc()
		}
	})
	reg.MustRegister(limiter.Collector()) // kudurru_ratelimit_buckets
}
stages := []pipeline.Stage{
	&ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version, Limiter: limiter},
	// ...
}

// Cache persistence: restore before the runner starts serving, then an
// always-on runnable that refreshes the dump and writes it at shutdown.
if rc != nil {
	cp, err := persist.New(localDB, persist.Options{}) // MaxAge 24h, gzip
	if err != nil { return err }
	if st, err := cp.Restore(ctx, rc); err != nil {
		logger.Warn("cache dump not restored", "err", err) // ErrStale / ErrIncompatible / cache.ErrBadDump
	} else if st.Entries > 0 {
		logger.Info("cache restored", "entries", st.Entries, "bytes", st.Bytes, "age", time.Since(st.WrittenAt).Round(time.Second))
	}
	add("cache-persist", false, func(ctx context.Context) error {
		return cache.RunPersister(ctx, rc, cp, 10*time.Minute, 5*time.Second, logger)
	})
}

// Multi-socket listeners.
registry, err := dnsserver.NewRegistry(dnsserver.Options{
	Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, Proxy: proxyPolicy,
	Sockets: cfg.Listeners.Sockets, // 0 = GOMAXPROCS
})
```

Close `localDB` after `runner.Run` returns. `RunPersister` returns before that, so its final save is complete by then.

### Config keys added (additive, drift test green)

| Key | Type | Scope | Default |
|---|---|---|---|
| `listeners.sockets` | int | node, hostOnly, restart | 0 (GOMAXPROCS) |
| `ratelimit.enabled` | bool | cluster | true |
| `ratelimit.ipv4` | map prefix length → QPM | cluster | `{32: 600, 24: 6000}` |
| `ratelimit.ipv6` | map prefix length → QPM | cluster | `{128: 600, 64: 6000, 56: 12000}` |
| `ratelimit.slip` | int 0-100 | cluster | 50 |
| `ratelimit.maxBuckets` | int | cluster | 65536 |
| `ratelimit.exempt` | string[] (CIDR or address) | cluster | `[]` |

The maps merge with the defaults key by key (koanf), so an operator switches a level off with `0`.

## Measurements

The machine has 8 vCPUs but a cgroup quota of 6, so Go 1.27 sets GOMAXPROCS=6. Four other agents were running at the same time (load average 2-5). Every number is from `go test` with no race detector.

| What | Result |
|---|---|
| Do53/UDP on loopback, local-answer stage, 1 socket (`BenchmarkUDPSockets`, 24 in-process clients) | 228.7k / 234.9k / 236.5k q/s (mean 233k) |
| Same, 6 SO_REUSEPORT sockets (GOMAXPROCS) | 348.8k / 359.1k / 361.8k q/s (mean 357k), **+53 %** |
| Same with `-cpu 8` (oversubscribed against the 6-CPU quota): 1 vs 8 sockets | 218.6k vs 325.6k q/s, +49 % |
| Lost queries in all socket runs | 0 |
| Rate limiter `Check`, parallel, 4096 clients, 2 IPv4 levels | 127 ns/op, 0 allocs |
| Cache persist, 200k packed A answers (197k resident), gzip (default) | blob 7.8 MB; Save 264 ms; Restore 336 ms |
| Same, uncompressed | blob 29.4 MB; Save 214 ms; Restore 177 ms |
| Binary (`make size`), before / after this package | 46,575,776 / 46,633,120 bytes (+57 KB); budget 51,234,000 (91 % used, 4.4 MiB headroom) |
| kube `LastContact` during a 3 s partition (envtest) | frozen; back 1.0 s after the partition healed |
| Slow-watcher conformance case | memory: closed, then a full re-list; sqlite: 1501/1501 on the open stream (4.2 s with race); kube: 1501/1501 (1.5 s) |

The clients share the CPUs with the server, so the socket gain is a lower bound; an external load generator would show more. gzip stays the default because the blob is 3.75 times smaller for about 50 ms more on Save and 160 ms more on Restore.

## Surprises

- **Metric name collision.** zonecache already registers `kudurru_store_watch_resyncs_total{kind}`, counting consumer re-lists after a watch closes, on the same registry serve uses. Registering the driver counter under the §11.1 name would have panicked at startup. The driver's feed catch-ups are therefore `kudurru_store_watch_catchups_total{kind}`, and a test pins that the two coexist. An architecture.md §11.1 edit should list both; this package may not touch that file.
- **SO_REUSEPORT defeats fail-fast port conflicts.** A second kudurru running as the same user would silently share the port. The plain probe bind keeps the old behaviour at the cost of one extra bind/close per listener at startup.
- **The memory driver ignored `WatchOptions.Namespace` for live events.** Fixed as part of the rewrite.
- **dnsproxy rewrites `h3://` addresses to `https://`** in `Upstream.Address()`, so the proto label reads the configured address.
- **envtest's apiserver waits for open watches on shutdown.** The kube cold-start test now closes the driver before stopping the control plane (it timed out otherwise), and the outage-under-a-running-driver case moved to a partitioning transport.
- **Pre-existing flake.** One full-suite run (all packages in parallel) failed `internal/controller` `TestKubeDriver`: the last SerialBumped Event was read before the recorder had emitted it. It passed 3/3 alone and is not related to these changes (not my package; added to followups). Every other package was green in that run, including the envtest suites for kube, zonecache and controller.
- **Upstream test changes.** Two existing upstream tests call `Requests.WithLabelValues` and now pass the new `proto` label (`"udp"`). That is the only edit to existing assertions. The removed kube helpers' tests were adapted as described above.

## Follow-ups (added to docs/followups.md)

- zonecache: `Touch` local.db with `Info().LastContact` instead of the wall clock.
- The cold-start cluster check needs an independent cluster ID while the apiserver is unreachable.
- Log observer: drops are counted as SERVFAIL responses; use an `rcode="none"` label.
- ratelimit: reload without a restart, a slip counter, per-ClientGroup exemptions.
- Multi-socket for DoT and the QUIC listeners.
- controller `TestKubeDriver` Event race.
- The fixed-runner bench job should also run `BenchmarkUDPSockets` and `BenchmarkPersist200k`.
- architecture.md §11.1: add `kudurru_store_watch_catchups_total`, `kudurru_store_watchers`, `kudurru_ratelimit_buckets` and the `ratelimit.*` / `listeners.sockets` keys to §6.2 and the decision log (integrator).
