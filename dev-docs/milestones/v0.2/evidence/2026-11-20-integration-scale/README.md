# SQLite integration scale diagnostic (#151 / #124)

Local synthetic evidence only; no reference hardware, hosted runner,
Cloudflare account, or shared cluster was used. The sandbox was
`c2a5eab6-78c` (Amazon Linux, Linux/amd64, Intel Xeon 2.90 GHz, one vCPU
of cgroup quota, 16 GiB memory limit, XFS), with Go 1.27.4 and
`GOMAXPROCS=2`. The measured source was main
`ce3ab6e2e28ad11663f1659f9e19fa74e5a33e61` plus the diagnostic and scale
fixture recorded in `7efef5b265bcf5f7c9150b28dae15a8573c116a3`. A cell is a
single observation, not a distribution. The VM was not a reserved benchmark
host; other local builds and tests could run during the sweep.

- `capture` is the real SQLite runtime's capture under a 10-second context.
- `apply` is a single changing `CommandTx.Apply` on the SQLite writer
  (standalone authority mode), including the command postimage read, with a
  30-second context.
- `cleanup` is wall time from the delete request to finalization by the
  production integration host, not the duration of an individual run. Its
  operations retain the 30-second deadline. The harness observes for up to
  two minutes after the delete request is accepted. A harness cutoff is not
  an operation deadline; the result retains the current reason and row counts.
- The mounted TLS Cloudflare double has one owned record set (two provider
  IDs) and a foreign row. The test checks exact delete IDs, no additional
  batch, foreign-row preservation, and removal of the owned ledger and
  operation rows without removing the shared budget. This is not a
  large-ledger or many-zone cleanup workload.
- The additional rows are disabled DNS record sets, with no extra padding
  (`small`), 4 KiB of annotation padding, or 32 KiB of annotation padding.
  Counts include the fixture's existing objects and padding is not the
  total serialized row size. `idle` adds no load; `loaded` adds two
  concurrent complete-generation readers, not external network traffic.

## Repeat one cell

```sh
export GOMAXPROCS=2 KUDURRU_INTEGRATION_SCALE=1
export KUDURRU_INTEGRATION_SCALE_REVISION=$(git rev-parse HEAD)
export KUDURRU_INTEGRATION_SCALE_CELL=1000:4k:loaded
go test -p 2 -count=1 -timeout 8m -v -run '^TestIntegrationScale$' ./cmd/kudurru
CGO_ENABLED=1 go test -race -p 2 -count=1 -timeout 8m -v -run '^TestIntegrationScale$' ./cmd/kudurru
```

Repeat serially for counts `1000`, `10000`, `25000`, `50000`, `100000`,
sizes `small`, `4k`, `32k`, and modes `idle`, `loaded`. When annotation
padding alone exceeds the generation byte limit, the fixture reports an
excluded capacity cell without writing that database. The seed limit is
a separate harness budget, not a production operation deadline.

## Coverage

See `matrix.log` for 12 timed cells and five padding-based exclusions. The
10,000-object small loaded run hit the 10-second capture deadline and did
not finalize by the cleanup cutoff. Some cutoffs had no owned ledger or
operation rows remaining but still held the finalizer; that is progress,
not a completed cleanup. Both race-detector samples are at 1,000 small
objects. Larger admissible shapes and the rest of the race/load matrix still
need measurement. Do not infer a universal supported count or close the
parent issue from these results.

The November 28 run measured the remaining admissible cells. Its
[README](../2026-11-28-integration-scale/README.md) lists every cell of
both runs in one table, with findings.

The unit and mounted tests for capture failure visibility also passed under
the race detector; their output is in `capture-tests.log`.
