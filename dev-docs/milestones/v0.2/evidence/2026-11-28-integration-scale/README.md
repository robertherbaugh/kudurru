# SQLite integration scale matrix, completed (#151 / #124)

This run completes the matrix that the
[November 20 partial run](../2026-11-20-integration-scale/README.md) started.
It uses the same harness, `TestIntegrationScale` in
`cmd/kudurru/integration_scale_test.go`, unchanged. No production bound,
harness cutoff or assertion was changed. This is local synthetic evidence:
no reference hardware, hosted runner, Cloudflare account or shared cluster
was used. Each cell is a single observation, not a distribution.

## Host and source

- Host `e6df2d6e-de3`: Amazon Linux 2023, Linux 6.18.56, amd64, Intel Xeon
  2.90 GHz, 8 vCPUs with no cgroup CPU quota, 16 GiB memory without swap, a
  64 GB XFS root.
- Go 1.27.4 with `GOMAXPROCS=2`, as on November 20.
- Source: main `e6ebe9fa4c3bde8ef75778d1587cf3a6d27673bb` with no local
  changes. The harness and its delete fixture
  (`cmd/kudurru/integration_scale_test.go`,
  `cmd/kudurru/integration_cleanup_test.go`) have not changed since the
  November 20 fixture revision `7efef5b2`, but the code under test has
  (for example the bounded cleanup progress of #204), so the two runs
  measured different revisions.
- The machine was shared with other agents' builds and focused tests. The
  1-minute load average at the start and end of every cell is in the table.
  Another agent's race-detector stress run overlapped the 50,000-object
  cells without the race detector (load up to 8.6). The first `25000:small:idle`
  run overlapped this run's own race-detector build, so that cell was run
  again; both samples are listed.

The November 20 host (`c2a5eab6-78c`) had one vCPU of cgroup quota. The two
hosts are not interchangeable, so the table keeps each sample's run.

## Method

The test binaries were built once and each cell was run on its own, one
after the other, from `cmd/kudurru`:

```sh
go test -c -o /tmp/kudurru.norace.test ./cmd/kudurru
CGO_ENABLED=1 go test -race -c -o /tmp/kudurru.race.test ./cmd/kudurru
cd cmd/kudurru
export GOMAXPROCS=2 KUDURRU_INTEGRATION_SCALE=1
export KUDURRU_INTEGRATION_SCALE_REVISION=e6ebe9fa4c3bde8ef75778d1587cf3a6d27673bb
KUDURRU_INTEGRATION_SCALE_CELL=25000:4k:loaded /tmp/kudurru.norace.test \
  -test.count=1 -test.timeout 12m -test.v -test.run '^TestIntegrationScale$'
```

This is the same test as the `go test -run '^TestIntegrationScale$'` recipe
in the November 20 README, without rebuilding for every cell. The test
timeout was 12 minutes instead of 8 because seeding 100,000 objects under the
race detector takes minutes; the test timeout is not a production bound.

Around each cell, a wrapper recorded the start and end time, free disk, the
load average and the binary's peak resident memory (`VmHWM`, sampled every
2 seconds). A separate guard would have killed a test binary if the host's
`MemAvailable` fell below 2 GiB, so that a large cell could not push the
shared machine into the kernel OOM killer. It fired once, during
`25000:4k:loaded` with the race detector, at 13.1 GiB peak resident memory
(`memory-guard.log` at the end of `matrix.log`).

The columns mean the same as on November 20:

- Capture: the harness's own call to the real SQLite runtime capture under a
  10-second context, as `duration/outcome`.
- Apply: one changing `CommandTx.Apply` on the SQLite writer, with the
  command postimage read, under a 30-second context.
- Cleanup: wall time from the delete request to finalization by the
  production integration host. `harness-cutoff` means the Integration was
  still present when the harness stopped observing two minutes after the
  delete request was accepted; it is not an operation deadline.
- Host capture failures: the host's `integration capture failed` log lines
  by reason, with the longest logged duration. The host logs a transition
  into a failed capture, so the count is the number of transitions, not the
  number of failed attempts. One or two `CaptureUnavailable` lines are
  expected: the host tries to capture before the fixture's writer is
  ready, and again while the fixture shuts down after the result line
  (these teardown captures can take several seconds).
- State at cleanup cutoff: the Integration's phase and reason plus the owned
  ledger and operation rows still stored. "No instance status" means the
  last capture had failed, so the host had forgotten instance status and
  reported only the capture reason, which is the designed behavior.

## Results

Every admissible cell now has a sample with and without the race detector,
except where noted. Raw logs for this run are in `matrix.log`; the
November 20 rows come from that run's `matrix.log`.

### Without the race detector

| Cell | Run | Capture | Apply | Cleanup | Host capture failures | State at cleanup cutoff | Peak RSS | Load (1 min) start/end |
|---|---|---|---|---|---|---|---|---|
| `1000:small:idle` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 468ms/finished | 46ms/finished | 2.602s/finished | CaptureUnavailable x2 | - | not recorded | not recorded |
| `1000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 570ms/finished | 27ms/finished | 3.436s/finished | CaptureUnavailable x2 | - | 0.1 GiB | 2.13/3.24 |
| `1000:small:loaded` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 548ms/finished | 91ms/finished | 5.011s/finished | CaptureUnavailable x2 | - | not recorded | not recorded |
| `1000:4k:idle` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 1.175s/finished | 97ms/finished | 5.703s/finished | CaptureUnavailable x1 | - | not recorded | not recorded |
| `1000:4k:loaded` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 1.112s/finished | 116ms/finished | 16.661s/finished | CaptureUnavailable x1 | - | not recorded | not recorded |
| `1000:32k:idle` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 4.865s/finished | 804ms/finished | 35.088s/finished | CaptureUnavailable x2 | - | not recorded | not recorded |
| `1000:32k:loaded` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 6.666s/finished | 953ms/finished | 2m5.107s/harness-cutoff | CaptureUnavailable x2 | Terminating/AuthorityChanged, ledger 0, ops 0 | not recorded | not recorded |
| `10000:small:idle` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 7.251s/finished | 421ms/finished | 30.393s/finished | CaptureUnavailable x1 | - | not recorded | not recorded |
| `10000:small:loaded` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 10.039s/deadline | 502ms/finished | 2m0.211s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.1s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 0, ops 0 | not recorded | not recorded |
| `10000:4k:idle` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 10.038s/deadline | 1.555s/finished | 2m0.011s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/AuthorityChanged, ledger 0, ops 0 | not recorded | not recorded |
| `10000:4k:loaded` | 2026-11-20 `c2a5eab6-78c` `ce3ab6e2+fixture` go1.27.4 | 10.016s/deadline | 3.893s/finished | 2m8.421s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 1, ops 1 | not recorded | not recorded |
| `25000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.017s/deadline | 1.155s/finished | 2m0.47s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 0, ops 0 | 1.1 GiB | 3.36/3.27 |
| `25000:small:idle` | 2026-11-28 (overlapped a build) `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.031s/deadline | 1.215s/finished | 2m0.381s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 0, ops 0 | 1.3 GiB | 3.24/3.05 |
| `25000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.01s/deadline | 2.566s/finished | 2m1.753s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.1s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 1, ops 3 | 1.2 GiB | 3.05/2.94 |
| `25000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.03s/deadline | 6.124s/finished | 2m0.007s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 10.2s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 1, ops 2 | 5.8 GiB | 2.94/3.82 |
| `25000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.031s/deadline | 5.328s/finished | 2m8.222s/harness-cutoff | CaptureDeadlineExceeded x3 (longest 13.1s), CaptureUnavailable x2 | Terminating/ProviderFailed, ledger 1, ops 1 | 6.2 GiB | 3.82/4.27 |
| `50000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.018s/deadline | 2.322s/finished | 2m0.342s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/AuthorityChanged, ledger 0, ops 0 | 2.4 GiB | 4.27/5.65 |
| `50000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.015s/deadline | 3.336s/finished | 2m2.171s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.1s), CaptureUnavailable x1 | Terminating/ProviderFailed, ledger 1, ops 1 | 2.4 GiB | 5.65/8.56 |
| `50000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 13.921s/deadline | 8.519s/finished | 2m0.441s/harness-cutoff | CaptureDeadlineExceeded x6 (longest 16.7s), CaptureUnavailable x1 | Terminating/Election, ledger 1, ops 1 | 10.7 GiB | 8.56/6.41 |
| `50000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 14.052s/deadline | 10.344s/finished | 2m12.044s/harness-cutoff | CaptureDeadlineExceeded x6 (longest 12.4s), CaptureUnavailable x2 | Terminating/Election, ledger 1, ops 1 | 11.6 GiB | 6.41/3.50 |
| `100000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.038s/deadline | 4.664s/finished | 2m1.232s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 1, ops 1 | 4.5 GiB | 3.50/2.78 |
| `100000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 11.364s/deadline | 9.783s/finished | 2m6.403s/harness-cutoff | CaptureDeadlineExceeded x6 (longest 15.1s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 4.8 GiB | 2.78/4.38 |

### With the race detector

| Cell | Run | Capture | Apply | Cleanup | Host capture failures | State at cleanup cutoff | Peak RSS | Load (1 min) start/end |
|---|---|---|---|---|---|---|---|---|
| `1000:small:idle` | 2026-11-20 `c2a5eab6-78c` `7efef5b2` go1.27.4 | 5.597s/finished | 614ms/finished | 2m0.019s/harness-cutoff | CaptureUnavailable x1 | Terminating/CleanupFailed, ledger 0, ops 0 | not recorded | not recorded |
| `1000:small:loaded` | 2026-11-20 `c2a5eab6-78c` `7efef5b2` go1.27.4 | 7.383s/finished | 774ms/finished | 2m0.131s/harness-cutoff | CaptureUnavailable x1 | Terminating/AuthorityChanged, ledger 0, ops 0 | not recorded | not recorded |
| `1000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.06s/deadline | 2.55s/finished | 2m0.012s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.1s), CaptureUnavailable x2 | Terminating/AuthorityChanged, ledger 0, ops 0 | 1.0 GiB | 3.27/2.09 |
| `1000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10s/deadline | 4.347s/finished | 2m4.161s/harness-cutoff | CaptureDeadlineExceeded x3 (longest 10.7s), CaptureUnavailable x2 | Terminating/ProviderFailed, ledger 1, ops 1 | 1.0 GiB | 2.09/2.30 |
| `1000:32k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 14.511s/deadline | 12.164s/finished | 2m0.03s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 3.7 GiB | 2.30/2.78 |
| `1000:32k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 15.748s/deadline | 24.538s/finished | 2m0.685s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 10.0s), CaptureUnavailable x2 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 4.3 GiB | 2.78/1.98 |
| `10000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 15.435s/deadline | 4.718s/finished | 2m0.016s/harness-cutoff | CaptureDeadlineExceeded x5 (longest 11.8s), CaptureUnavailable x2 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 1.6 GiB | 1.98/2.09 |
| `10000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 16.799s/deadline | 7.541s/finished | 2m13.026s/harness-cutoff | CaptureDeadlineExceeded x7 (longest 13.7s), CaptureUnavailable x2 | Terminating/Election, ledger 1, ops 1 | 1.5 GiB | 2.09/2.09 |
| `10000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 24.363s/deadline | 21.763s/finished | 2m0.026s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 24.5s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1; test FAILED at teardown: `complete runtime failed to join` (10 s) | 4.7 GiB | 2.09/1.95 |
| `10000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 33.135s/deadline | 36.306s/deadline | 2m0.067s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 30.7s), CaptureUnavailable x2 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 6.3 GiB | 1.95/2.09 |
| `25000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 18.184s/deadline | 12.706s/finished | 2m0.014s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 19.6s), CaptureUnavailable x2 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 2.9 GiB | 2.09/2.13 |
| `25000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 22.048s/deadline | 28.299s/finished | 2m4.982s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 17.0s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 3.0 GiB | 2.13/2.36 |
| `25000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 10.067s/deadline | 55.05s/deadline | 2m0.046s/harness-cutoff | CaptureDeadlineExceeded x2 (longest 28.7s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 11.5 GiB | 1.13/2.06 |
| `25000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | killed by the host memory guard before the result line | - | - | CaptureDeadlineExceeded x2 (longest 10.0s), CaptureUnavailable x1 | - | 13.1 GiB | 2.06/1.87 |
| `50000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 15.115s/unavailable | 31.424s/deadline | 2m0.037s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 33.8s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1 | 5.5 GiB | 2.36/1.89 |
| `50000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | 12.315s/unavailable | 42.774s/deadline | 2m0.19s/harness-cutoff | CaptureDeadlineExceeded x1 (longest 13.3s), CaptureUnavailable x1 | no instance status (CaptureDeadlineExceeded), ledger 1, ops 1; test FAILED at teardown: `complete runtime failed to join` (10 s) | 6.8 GiB | 1.89/2.14 |
| `50000:4k:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | not reached: seed hit the 5 min harness seed budget after 5m41.844s | - | - | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x2 | - | 8.5 GiB | 1.87/1.26 |
| `50000:4k:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | not reached: seed hit the 5 min harness seed budget after 5m18.539s | - | - | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x2 | - | 5.6 GiB | 1.26/1.20 |
| `100000:small:idle` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | not reached: seed hit the 5 min harness seed budget after 5m0.007s | - | - | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x1 | - | 1.6 GiB | 2.14/1.23 |
| `100000:small:loaded` | 2026-11-28 `e6df2d6e-de3` `e6ebe9fa` go1.27.4 | not reached: seed hit the 5 min harness seed budget after 5m0.006s | - | - | CaptureDeadlineExceeded x1 (longest 10.0s), CaptureUnavailable x2 | - | 1.8 GiB | 1.23/1.13 |

### Excluded by the generation byte limit

`10000:32k`, `25000:32k`, `50000:32k`, `100000:32k` and `100000:4k`: the
annotation padding alone exceeds the 256 MiB generation limit, so the
fixture reports a capacity exclusion without writing the database
(November 20 `matrix.log`). These shapes cannot be stored, so a capture,
command or cleanup of them is not admissible.

## Findings

No bound was relaxed. Each finding below is a measurement against the
unchanged production bounds.

1. **Capture misses its 10-second bound from 25,000 objects.** Without the
   race detector, every cell at 25,000 objects or more, small or 4 KiB, idle
   or loaded, failed the harness capture and the host's own captures with
   `CaptureDeadlineExceeded`. With the November 20 samples, the largest
   shape that captured in time is 10,000 small objects at idle (7.3 s on the
   one-vCPU host); 10,000 small objects under load and 10,000 objects at
   4 KiB did not. With the race detector, every measured cell except
   1,000 small objects missed the capture deadline in the harness or the
   host.
2. **Failed captures return well after the 10-second bound (product
   finding).** The capture context is passed to the SQLite queries but is
   not checked by the work after them, so a capture that is already past
   its deadline keeps running until it reaches a step that checks. The
   longest logged host captures were 16.7 s (`50000:4k:idle`), 15.1 s
   (`100000:small:loaded`) and 13.1 s (`25000:4k:loaded`) without the race
   detector, and 33.8 s (`50000:small:idle`), 30.7 s (`10000:4k:loaded`)
   and 28.7 s (`25000:4k:idle`) with it. The harness's own capture returned after
   13.9 s and 14.1 s at `50000:4k` without the race detector. Code that does
   not check the context: `readGeneration` seals and marshals the whole
   image after its queries (`internal/store/sqlite/replication.go`), and
   `integrationRuntime.capture` decodes every object and prepares the zone
   cache (`cmd/kudurru/integration_runtime.go`). Where the time goes was not
   profiled. This is not fixed here.
3. **A changing command can exceed its 30-second bound and return late
   (product finding, race-detector cells).** Without the race detector, the
   longest Apply was 10.3 s (`50000:4k:loaded`). With it, Apply reported
   its deadline at `10000:4k:loaded` (36.3 s), `50000:small:idle` (31.4 s),
   `50000:small:loaded` (42.8 s) and `25000:4k:idle` (55.1 s), and
   `25000:small:loaded` finished at 28.3 s. The race detector's slowdown is
   a tooling factor, but returning up to 25 seconds after the deadline
   looks like the same pattern as finding 2, presumably in the command's
   whole-generation postimage read. Not profiled, not fixed here.
4. **Two race-detector cells failed at teardown.** `10000:4k:idle` and
   `50000:small:loaded` logged their results, then failed with `complete
   runtime failed to join` (the fixture's 10-second join after cancel in
   `cmd/kudurru/serving_runtime_test.go`). This fits findings 2 and 3:
   capture or command work in flight does not stop on cancel. The fixture
   join was not extended.
5. **Cleanup does not finish once capture fails.** No cell whose capture
   failed finalized its Integration within the two-minute window. Usually
   one owned ledger row and one operation row remained. This is the
   designed effect of a failed capture (it stops local jobs, including
   cleanup), so from 25,000 objects on this host (from 10,000 under load,
   and 10,000 at 4 KiB at idle, on the November 20 host) a deleted
   Integration did not finalize within the window.
6. **Admissible shapes need most of a 16 GiB host.** The test process's
   peak resident memory, which includes the fixture, seeding and the two
   load readers and is not a server measurement, was 1.1 to 1.3 GiB at
   25,000 small objects, 2.4 GiB at 50,000, 4.5 to 4.8 GiB at 100,000,
   5.8 to 6.2 GiB at `25000:4k` and 10.7 to 11.6 GiB at `50000:4k` (about
   200 MiB of padding, inside the 256 MiB generation limit). With the race
   detector, `25000:4k:idle` peaked at 11.5 GiB and `25000:4k:loaded` was
   killed by the memory guard at 13.1 GiB.
7. **Four race-detector cells could not be measured.** `50000:4k` and
   `100000:small`, idle and loaded, did not finish seeding within the
   harness's 5-minute seed budget (5m0s to 5m42s), so the harness reported
   a seed deadline and measured nothing. The seed budget is a harness
   limit, not a production bound, and it was not changed. Because these
   cells already fail capture without the race detector, the missing race
   samples would not change the conclusions above.
8. **Capture reported `unavailable` at `50000:small` with the race
   detector.** The harness capture returned `ErrUnavailable` after 15.1 s
   (idle) and 12.3 s (loaded), which is what capture returns when the
   serving authority or publication check after the generation read fails.
   This is recorded as observed; it was not investigated further.

## Proposed supported-scale statement (needs owner sign-off)

This is a draft for the owner's scale decision in #124 (option A, "document
what passes"). It is not approved and is not in the deploy docs.

> With SQLite storage, integrations are supported for stores of up to
> 1,000 objects whose records carry up to 4 KiB of annotations each. In
> that range, the 10-second capture, a changing command and a Cloudflare
> record-set cleanup finished in every sample without the race detector,
> idle and with two concurrent whole-generation readers. Stores between
> 1,000 and 10,000 objects are best effort: 10,000 small objects captured
> at idle on a one-vCPU host but missed the capture deadline under load,
> 10,000 objects with 4 KiB of annotations missed it at idle, and 1,000
> objects with 32 KiB of annotations did not finish cleanup under load.
> From 25,000 objects up, every sample missed the capture deadline, so the
> host stopped local integration jobs, including the cleanup of a deleted
> Integration. The 100,000-object generation limit is not a supported
> integration scale.

The option B target of a fixed count such as 100,000 objects is not met by
the current design. Meeting it would need a capture and command path that
does not have to read and decode the whole generation inside its deadline,
plus fixes for the deadline overruns in findings 2 and 3. These are local synthetic
samples, one per cell, not a hardware qualification.

## Rerun after the deadline fix (#281)

Findings 2 and 3 were fixed in #281 (D428): the whole-generation read, the
image validation, the capture's decode, zone cache and source loops, and the
wait to publish the captured cut now observe the operation's deadline. Four
cells were rerun on the same host and harness, without the race detector,
first on main `f99a340e` (no Go change since `e6ebe9fa` affects these
paths) and then with the fix, one cell at a time with the same wrapper
(`GOMAXPROCS=2`, peak resident memory and load recorded). The
`25000:small:idle` run on main also wrote a CPU profile.

| Cell | Code | Capture | Apply | Host capture failures (longest) | Peak RSS | Load (1 min) start/end |
|---|---|---|---|---|---|---|
| `25000:small:idle` | main | 10.058s/deadline | 1.218s/finished | CaptureDeadlineExceeded x1 (10.09s) | not recorded | 3.22/- |
| `25000:small:idle` | fix | 10.02s/deadline | 946ms/finished | CaptureDeadlineExceeded x1 (10.06s) | 1.2 GiB | 2.42/3.86 |
| `25000:4k:loaded` | main | 10.029s/deadline | 7.829s/finished | CaptureDeadlineExceeded x2 (11.94s) | 6.3 GiB | 1.41/1.95 |
| `25000:4k:loaded` | fix | 10.049s/deadline | 5.089s/finished | CaptureDeadlineExceeded x3 (10.06s) | 6.2 GiB | 3.86/3.99 |
| `50000:small:loaded` | main | 10.07s/deadline | 6.762s/finished | CaptureDeadlineExceeded x2 (10.63s) | 2.3 GiB | 1.95/3.56 |
| `50000:small:loaded` | fix | 10.028s/deadline | 2.727s/finished | CaptureDeadlineExceeded x3 (10.04s) | 2.3 GiB | 3.99/4.32 |
| `50000:4k:idle` | main | 13.921s/deadline | 9.296s/finished | CaptureDeadlineExceeded x7 (14.02s; 10.0, 10.6, 14.0, 10.3, 10.3, 12.1, 12.4) | 10.8 GiB | 2.99/3.55 |
| `50000:4k:idle` | fix | 12.418s/deadline | 8.735s/finished | CaptureDeadlineExceeded x6 (10.21s; 10.2, 10.1, 10.0, 10.0, 10.0, 10.0) | 9.5 GiB | 4.32/4.03 |

With the fix every logged host capture returned within 0.21 s of its
deadline. The harness's own capture at `50000:4k:idle` is the first
validation of a new generation, with no remembered canonical form or
acceptance, and it still returned 2.4 s late: instrumented runs put that
time in the single JSON decode of the whole image envelope in
`replication.Decode` (up to 3.8 s past the deadline at this shape), which
the fix does not split. The same instrumentation found a capture that had
finished in time and then waited about five seconds to publish behind a
command holding the current cut; the fix bounds that wait by the capture's
deadline. Captures and cleanup still miss their deadline at these sizes:
the fix makes them stop on time, not fit in it.
