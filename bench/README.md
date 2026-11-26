# Benchmark evidence

`make bench` runs each blocking and zone sample in a fresh Go process, five
processes per scope by default. It preserves native failures in the combined
output and invokes `hack/benchgate` against the committed baseline and limits.
The gate requires exactly `BENCH_COUNT` values for every metric in both current
and baseline inputs, including informational metrics. A partial sample cannot
silently reduce a median's count.

The parser rejects malformed result rows, duplicate units, non-finite or negative
measurements, invalid iteration counts and inconsistent CPU headers. Thresholds
and absolute limits must also be finite and nonnegative. These checks do not
change relative thresholds, noise floors, ignore rules or absolute ceilings.

To validate an already retained five-process run with the original six-processor
setting and matching reported CPU model:

```sh
go run ./hack/benchgate -baseline bench/baseline.txt -limits bench/limits.txt \
  -threshold 20 -samples 5 -procs 6 -strict-cpu path/to/current.txt
```

`-samples` counts measurements; it cannot prove they came from fresh processes.
Keep the original command and process receipts with the logs. `-procs` checks
every benchmark suffix in both inputs (no suffix means one processor).
`-strict-cpu` requires CPU headers and compares against the baseline when supplied. Matching
CPU text and processor count do not establish a named fixed runner class.

The committed baseline and limits cover blocking and zone lookup. New metrics
are reported separately; they do not acquire calibrated performance limits merely
by appearing in the output. Cache/strategy, socket and persistence qualification
on the selected fixed runner remain tracked in the v0.2 completion ledger. Preserve
failed runs when fixing code; do not replace samples or reset the baseline to
clear a regression.

## Additional release workloads

The manual **release benchmark measurements** workflow also collects the
response cache, upstream strategies, UDP/DoT/QUIC sockets, and `Persist200k`.
The owner selected **Ubicloud 8-core Ubuntu 24.04** on November 8, 2026. The
workflow pins `ubicloud-standard-8-ubuntu-2404` and retains `GOMAXPROCS=6`, the
original benchmark processor count. The eight-core host does not change the
workloads to eight processors. The same class and processor count apply to
baseline calibration and candidate comparison.

For an in-repository pull request, adding the `release-benchmarks` label starts
one explicitly requested recording of its exact head commit. Later pushes do
not repeat that measurement automatically. This opt-in path works before the
workflow is registered on the default branch; fork pull requests cannot use it.
The manual workflow remains available after registration. Both paths retain
the original workloads and leave the baseline and limits unchanged.

The workflow records measurements; a successful job is not release qualification.
Cache/strategy and socket/persistence bounds below are calibrated. The
final-source comparison remains open; the closure rule below (D383) says how
its one run is counted. Local sandbox
measurements, matching CPU text or merely passing this label to the script
cannot close the hosted gate.

For a clean committed checkout on the selected runner:

```sh
python3 hack/bench-release.py --runner-class ubicloud-standard-8-ubuntu-2404 --procs 6 \
  --output bin/release-benchmark-recording
```

The collector runs each suite five times in separate sequential Go processes,
with the existing benchmark bodies and corpora. UDP retains its documented
five-second duration; other cases use the original one-second default. Each
process has a 30-minute test timeout, an outer deadline, a 2 MiB log bound,
and disk admission/stop floors of 3/2 GiB. Numeric exit, wall time and sampled
minimum free space are synced before enrichment. No failed sample is retried
or replaced. Ordinary child failures remain in combined reports and fail the
result; a resource stop, changed source or leftover child holds the sequence.
Existing output directories are refused.

Reports separate blocking/zone rules from the additional suites, so ignores
for the blocking memory helper cannot hide cache or persistence measurements.
The original Go gate validates all report shapes and the unchanged
blocking/zone absolute limits plus the calibrated cache/strategy and
socket/persistence bounds below. UDP and TLS query throughput (`q/s`) is higher-is-better; loss
remains lower-is-better.

After separately reviewing and retaining a baseline artifact, compare on the
same actual runner:

```sh
python3 hack/bench-release.py --runner-class ubicloud-standard-8-ubuntu-2404 --procs 6 \
  --baseline /path/to/reviewed-artifact --output bin/release-benchmark-current
```

The baseline must contain every suite, successful measurements and gates,
matching class, platform, Go version, commands, sample count, processor count,
fixture/test source identities, and original limits identity. Its report hashes are checked
before and after measurement. Intentional fixture changes require explicit
baseline review, even if they affect only another test in the same package.
Each Go comparison requires five samples, matching processor suffixes and
matching CPU headers in both inputs. Empty, failed or incomplete baselines
refuse. No command updates a baseline. `result.json` keeps
`release_qualified: false`: these measurements alone do not establish the
external acceptance or other release gates.

## Original reference on the selected class

The first actual Ubicloud recording, [run 42514599389](https://github.com/robertherbaugh/kudurru/actions/runs/42514599389),
measured candidate `9cc2ae0423070fc514c94a25ab5b92e3816889e7` with Go 1.27.3
and six processors. Its AMD EPYC 9454P CPU differs from the retained Intel Xeon
baseline. All 40 native commands and seven shape/absolute checks passed: 136
cases, 592 metric keys, 2,960 values. This recording is not a baseline reset or
release qualification. The original Intel results and failures remain retained.

To obtain a reference on the selected class, add `release-benchmark-reference`
to the in-repository PR, or select `reference` in the manual workflow. This
measures immutable commit `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2`, which last
established the original committed baseline. Its blocking and zone benchmark
bodies match the candidate except for documentation comments; its original
cache and strategy cases also remain. Later added cases remain new metrics.

The historical source and current measurement controls occupy separate clean
checkouts. Twenty fresh native processes cover the four original scopes, with
the original one-second durations, five samples and six processors. The current
strict gate checks the three resulting reports. Raw process receipts and both
source manifests accompany the artifact. Any changed source, resource stop or
remaining descendant stops the sequence; native failures are retained without
retries or replacement samples. Nothing edits `bench/baseline.txt`.

Reference recording alone does not close the gate. Its actual results must be
reviewed and compared with the candidate on this class under the unchanged
relative threshold and noise floors. The cache/strategy calibration below and final-current-source qualification
remain separate from that original recording.


## Paired measurements in one VM

The selected Ubicloud pool is heterogeneous: the candidate recording above
used EPYC 9454P, while original-reference run 42517712186 used EPYC 9224. The
reference also failed its unchanged CNAME p99 ceiling (2,090 ns > 2,000 ns).
Those failures remain retained; the two artifacts are not a valid CPU-matched
comparison.

The `paired` workflow mode (or `release-benchmark-paired` label on an internal
PR) runs the immutable original reference and exact candidate in one VM.
It retains twenty original-reference commands, forty candidate commands, and
their original shape/absolute gates. D412 below interleaves matched scopes;
historical runs through 43685577680 recorded the entire reference first. Both
complete source manifests and every numeric process receipt are retained. No
failed sample is retried, replaced, or selected for its CPU or outcome.

Only a complete reference with successful native commands and all three gates
can enter the three original-scope comparisons. A failed reference still permits
the independent candidate recording, but refuses comparison and fails the paired
result. The original Go gate retains five samples, six processors, strict CPU
matching, the 20% threshold, noise floors and absolute limits. Source changes,
resource stops, residual descendants and altered artifacts stop remaining work.
Candidate socket/persistence scopes also enforce the initial hosted calibration
below. `release_qualified` remains false pending resolution of measured failures,
final-source qualification and all other release requirements.
Neither collector changes the committed Intel baseline or treats the candidate
as its own reference.


## Cache and strategy ceilings

`cache-limits.txt` and `upstream-limits.txt` pin all 20 original cache metrics
and 60 original strategy metrics to independently recorded historical source
0cc9565e on the selected Ubicloud class (EPYC 9224, Go 1.27.3, six processors).
Each ceiling is its five-process median plus the larger of 20% or the existing
per-unit noise floor. The original vectors, exact reports, all ten native
receipts and both successful scope gates are in `calibration/ubicloud8-20261108/`.
Control tests verify hashes, distinct processes, metric completeness and the
formula. These ceilings do not alter any original blocking/zone rule or ignore.

This is scoped calibration, not acceptance of the full historical reference:
that reference's separate CNAME absolute gate failed. It remains ineligible for
the paired baseline comparison unless a complete paired recording passes all
its required gates. The collector pins all seven limit-file identities, applies the applicable
limits to recording and comparison, and rejects a baseline with different limits. Later cache Growth cases remain new; no old baseline is invented for them.

Applying the new ceilings to retained candidate run 42514599389 fails five cache
metrics (parallel hit, insertion bytes, three memory metrics) and three strategy
latencies. Its CPU also differs from the historical reference, so these absolute
observations are not a strict relative comparison. The failures remain open;
limits must not be raised to clear them. The paired original comparison and
final-source release qualification remain open.


The first paired execution, [run 42518988512](https://github.com/robertherbaugh/kudurru/actions/runs/42518988512),
measured original `0cc9565e` and candidate `92d5c9ee` on the same EPYC9454P VM.
All sixty native measurements and ten shape/absolute gates passed. The actual
strict Go comparisons failed blocking/zone (one exact-miss p50 regression) and
cache (eight latency/allocation/memory regressions); all sixty original upstream
strategy metrics passed. The job correctly failed and `release_qualified` stayed
false. This run predates the compact cache metadata fix and the additional
cache/strategy limits above. Current-source measurement is still required;
prior failed runs and all five-value vectors remain retained in the release ledger.


The next paired execution, [run 42522360297](https://github.com/robertherbaugh/kudurru/actions/runs/42522360297),
measured original `0cc9565e` and compact-metadata candidate `91cb21bb` together on
one Ryzen 9 7950X3D VM in the same selected class. The strict comparisons passed
blocking/zone and upstream, and failed two cache latency metrics: hit 191.1 vs
158.2 ns and Set 313.3 vs 258.1 ns. Memory passed at 308.7 vs 292.7 bytes/entry.
All 811 five-value vectors and numeric outcomes remain retained. This confirms
the pool's CPU variation and the need for comparisons within one VM; it does not
erase earlier failures or qualify later source changes.

Paired [run 42530394856](https://github.com/robertherbaugh/kudurru/actions/runs/42530394856)
measured candidate `b2d197d8` on one EPYC 9454P VM. The historical reference
itself failed two upstream slow-first/rtt_weighted limits there, so no relative
comparison ran (`comparison_eligible: false`); the candidate also failed cache
parallel hit (177.1 against 156.5 ns/op) and six upstream limits.

Paired [run 42545184734](https://github.com/robertherbaugh/kudurru/actions/runs/42545184734)
measured candidate `73d4b057` on one EPYC 9454P VM. The reference passed its
gates, so the comparison was eligible, and it failed: seven blocking/zone, one
cache and ten upstream relative regressions, plus the cache parallel-hit
(173.6 against 156.48 ns/op) and six upstream absolute limits.

Paired [run 42554034011](https://github.com/robertherbaugh/kudurru/actions/runs/42554034011)
measured candidate `9963dec2` on one Ryzen 9 7950X3D VM and passed all three
strict relative comparisons, all seven candidate absolute gates and the three
reference gates. Cache hit was 105.7 against 110.8 ns, Set 191.3 against
206.6 ns and parallel hit 30.09 against 28.87 ns (candidate against reference).
The regex-1000 miss p99 was 871 ns against 8,376 ns, and 1,389 against
11,390 ns in the failed EPYC run above, so the earlier adverse regex result is
superseded on both CPUs. This is measured performance acceptance for
`9963dec2` on that CPU only: the earlier failures stay retained, and the
cache fix of D348 has not yet been measured on an EPYC VM.

## Recorded runs

| Run | Mode | Source | CPU | Outcome |
| --- | --- | --- | --- | --- |
| [42514599389](https://github.com/robertherbaugh/kudurru/actions/runs/42514599389) | candidate | `9cc2ae04` | EPYC 9454P | recorded; 40 commands and 7 shape/absolute checks passed; calibrates the socket/persistence bounds |
| [42517712186](https://github.com/robertherbaugh/kudurru/actions/runs/42517712186) | reference | `0cc9565e` | EPYC 9224 | recorded; CNAME p99 ceiling failed (2,090 > 2,000 ns); calibrates the cache/strategy ceilings |
| [42518988512](https://github.com/robertherbaugh/kudurru/actions/runs/42518988512) | paired | `92d5c9ee` | EPYC 9454P | failed: 1 blocking/zone and 8 cache regressions; upstream passed |
| [42522360297](https://github.com/robertherbaugh/kudurru/actions/runs/42522360297) | paired | `91cb21bb` | Ryzen 9 7950X3D | failed: cache hit and Set latency; blocking/zone and upstream passed |
| [42530394856](https://github.com/robertherbaugh/kudurru/actions/runs/42530394856) | paired | `b2d197d8` | EPYC 9454P | failed: reference failed its upstream limits (not comparable); candidate cache and upstream limits failed |
| [42545184734](https://github.com/robertherbaugh/kudurru/actions/runs/42545184734) | paired | `73d4b057` | EPYC 9454P | failed: 18 relative regressions, cache and upstream absolute limits |
| [42554034011](https://github.com/robertherbaugh/kudurru/actions/runs/42554034011) | paired | `9963dec2` | Ryzen 9 7950X3D | passed: 3 relative comparisons, 7 candidate and 3 reference absolute gates |
| [43685577680](https://github.com/robertherbaugh/kudurru/actions/runs/43685577680) | paired | `21018c6c` | Ryzen 9 7950X3D | failed: suffix-hit p99 and policy-hit average; cache/upstream comparisons and all 10 absolute gates pass |

Through source `9963dec2`, all paired runs that passed blocking/zone and
upstream used Ryzen VMs, while every EPYC run failed, twice with blocking, zone
and upstream code byte-identical to a passing source. Host variation was an
inference from these results, not a measured cause.

The November 15 measurement on `21018c6c` also fails on Ryzen. With Go 1.27.4,
reference and candidate pass all 60 native processes and all ten absolute gates.
The strict blocking/zone comparison fails exactly two metrics:

| Metric | Reference median | Candidate median | Change |
| --- | ---: | ---: | ---: |
| `BenchmarkLatency/suffix-1M/hit` p99 | 320 ns | 511 ns | +59.7% |
| `BenchmarkEvaluate/policy-3M/hit` average | 226.8 ns | 361 ns | +59.2% |

Policy-hit averages span `[225.5, 224, 226.8, 229.7, 460.6]` ns in the
reference and `[361, 231, 381, 241.7, 419.2]` ns in the candidate. Suffix-hit
p99 spans `[321, 320, 311, 310, 351]` and `[511, 611, 311, 361, 671]` ns.
The blocking production and benchmark implementation is unchanged from
`9963dec2` (which used Go 1.27.3); one unrelated correctness test changed.
Neither the spread nor source equality establishes the cause. The recording
contains no CPU-affinity or topology evidence. The failed run remains the D383
measurement of record, and the benchmark release gate remains open.

## Closure rule for the mixed runner pool (D383)

This rule is recorded before the final measurement, so its outcome is not
interpreted after the fact:

- The final head gets exactly one paired run, with the unchanged collector,
  workloads, threshold, noise floors and limits. That run is the measurement of
  record whatever CPU it draws. Its CPU model is recorded next to its outcome.
- A pass closes the benchmark element of G23 for that head and that CPU model,
  and says so: a pass on Ryzen does not show the EPYC results superseded.
- A failure is retained as a failure on any CPU. The job is not run again to
  draw another VM, no sample is replaced or selected, and no limit is raised or
  recalibrated from the failing candidate. The failing metrics are diagnosed
  from the reviewed vectors first. On EPYC, the diagnosis quotes
  `BenchmarkGet/hit-parallel`, `BenchmarkStrategies/fast/strict`,
  `BenchmarkLatency/stevenblack/miss` and `BenchmarkLookup100k/cname` p99
  against the run 42545184734 values on `73d4b057`.
- A code fix for a measured regression, or a change of method (for example
  interleaving reference and candidate processes), is a new source or a new
  decision followed by a new run; the failed run stays in this table.
- A source change in a benchmarked package after the run voids it for the
  final head. Documentation-only commits do not.

### Interleaved recording decision (D412)

Run 43685577680 remains failed. Its cause is unconfirmed: neither identical
blocking production code at a previously passing source nor local diagnostics
establish why it failed. Recording every reference process before every
candidate process leaves elapsed host conditions confounded with source.
D412 changes that order before the next hosted recording, without attributing
the failure to a particular host condition.

For sample numbers one through five, each original scope (blocking, zone,
cache, upstream) runs its reference and candidate consecutively. Reference goes
first when the sum of the one-based sample number and zero-based scope index
is odd; candidate goes first otherwise. After all forty matched commands,
the twenty candidate-only UDP, DoT, QUIC and persistence commands run in the
existing sample/suite order. The original ten absolute gates and three eligible
relative comparisons follow. The complete sequence, source manifests, Go
version, Linux allowed CPU mask and CPU topology are recorded before timing.
Completed native logs and receipts are hashed and checked throughout the run.

All sixty native commands, benchmark bodies, corpora, durations, five-process
sample counts, six-processor environment, immutable original reference,
thresholds, noise floors and limits remain unchanged. Failed native commands
continue through the declared sequence and fail their recording; resource
stops, source changes and altered evidence hold the remaining work. There are
no retries or replacement samples. `--order sequential` preserves the old
method; the workflow explicitly selects `--order interleaved`.

The next single hosted interleaved run is counted for this method whatever its
CPU and outcome. A pass qualifies only the named source and CPU under D412;
it does not erase the sequential failures or prove a product speedup. A failure
keeps the release gate open. D383 still forbids a rerun just to obtain another
VM or result. No baseline or acceptance limit is reset.

#### Host-equivalence preflight (proposed)

D416 proposes an extra check for dedicated-host `qualify`; it is off by
default and not adopted. Do not use it for qualification or enable it in a
workflow without owner approval. The command after adoption would be:

```sh
python3 -B hack/bench-dedicated.py qualify \
  --reference "$ORIGINAL_CHECKOUT" --host-record "$REVIEWED_HOST" \
  --calibration "$CALIBRATION_DIR" --approval "$CALIBRATION_APPROVAL" \
  --host-equivalence-preflight "$PREFLIGHT_APPROVAL" --output "$NEW_OUTPUT_DIR"
```

When the qualification host differs from calibration, also supply the
separately reviewed `--host-transition-approval`. The preflight approval needs
`decision: accepted`, `reviewed_by`, `reviewed_at`, `review_evidence`, and
`purpose: host-equivalence-preflight`. It binds `calibration_manifest_sha256`,
`calibration_approval_sha256`, `qualification_host_sha256`, and `method_sha256`
(the SHA-256 of `hack/bench-dedicated.py`). The approval is rehashed before and
after each measured process.

Only the original reference runs: one process each for blocking, zone, cache,
and upstream at six processors. The native gate checks the combined
blocking/zone report against `bench/limits.txt` and the other two reports
against the reviewed calibration's `cache-limits.txt` and `upstream-limits.txt`.
It uses `-samples 1 -procs 6 -strict-cpu` (a CPU header is required), with no
baseline or relative comparison. The fixed-product file bounds matching
metrics; it is not a complete metric inventory check. Preflight builds and
runs the gate directly, so its exit codes distinguish bounds failures (1)
from invalid or incomplete input (2).

`<output>/preflight/` retains build, process, and gate logs and receipts, the
gate binary if built, assembled reports, and `result.json` with
`preflight_pass`, `held`, `outcome`, `gate_exits`, and `receipt_sha256`.
Both `counted` and `release_qualified` are always false there.
Only a pass starts the unchanged interleaved engine in `<output>/paired/`;
preflight values never enter its five-process vectors. A bounds failure
(`outside_bounds`) or benchmark or gate resource stop (`resource_stop`) holds
without starting paired timing. Operator, tooling, or integrity failures are
retained as `refused`, not a host hold; an interrupted run can remain
`incomplete`. Neither outcome establishes anything about the host or authorizes
a retry. Use a new output path outside the source checkouts; existing output is
refused. The proposal retires a held host without counting the hold under D383;
a new host requires a new reviewed transition record, not an automatic redraw
or retry.

A pass only shows these original-reference reports met the specified bounds.
One process per scope is noisier than the five-process median used for
calibration: a marginal host can pass or hold by chance. It does not establish
host equivalence, physical isolation, candidate performance, or release
qualification. The owner still needs to adopt the method, including deciding
whether to retire held hosts and whether one process per scope is sufficient.


## Socket and persistence bounds

`udp-limits.txt`, `dot-limits.txt`, `quic-limits.txt` and `persist-limits.txt`
use the first complete hosted recording, run 42514599389 at immutable source
`9cc2ae0423070fc514c94a25ab5b92e3816889e7`. It used the selected Ubicloud class,
EPYC 9454P, Go 1.27.3 and six processors. The retained 14 cases contain 58
metrics and 290 values from five fresh processes per suite. Exact reports,
commands, receipts, workload identities and all vectors are preserved in
`calibration/ubicloud8-sockets-persist-20261108/`. These workloads have no original
committed baseline; this initializes their bounds without changing the original
blocking/zone, cache or strategy references.

The limits file supports these independent rules:

- `limit <regexp> <unit> <maximum>` bounds the median above. Each initial ceiling
  is the recorded median plus the larger of 20% or the existing unit noise floor
  (25 ns/op, 64 B/op, otherwise zero).
- `minimum <regexp> <unit> <minimum>` bounds the median below. Throughput and
  persisted entry counts must retain at least 80% of the initial median; a
  smaller completed persistence workload cannot pass merely by finishing faster.
- `require <regexp> <unit>` rejects an absent metric, independently of ignores.
  The calibration requires every exact benchmark/unit pair.
- `zero <regexp> <unit>` rejects any nonzero sample. UDP loss and DoT failure
  counters must be zero in all five processes, even when their median is zero.

Seventeen significant digits preserve the computed boundaries. The original
relative 20% threshold, noise floors, ignores and baseline files are unchanged.
The newer retained candidate recording 42522360297 passes all four actual Go
scope gates; this does not erase its separate cache comparison failure. New
current-source measurements remain required. Never replace a failing sample,
raise a bound, or recalibrate from a failing candidate to clear a gate.

## Upstream diagnostics after D412

The failed interleaved run remains the measurement of record. The separate
`diagnostic` mode (or `release-benchmark-diagnostic` label on an internal PR)
collects six predeclared native processes on the same selected runner class,
Go 1.27.4 and six processors. It runs the original fast/slow-first strict and
RTT-weighted cases: reference/candidate, candidate/reference, then a trace for
each source. The trace is instrumented; its timings are not qualification data.

`hack/bench-diagnostic.py` writes the full plan before timing and preserves two
unprofiled processes plus one trace per source, complete source manifests,
receipts and CPU pressure/throttling counters before/after each process. It
reuses the existing disk/log/deadline/process-group controls and holds remaining
work on source changes or resource stops. Native failures are retained without
retry. It reads no baseline, runs no qualification gate and always records
`release_qualified: false`. A diagnostic success cannot close D383/D412, reset a
baseline or supersede a failed formal recording. A causal fix or a separately
reviewed method change remains necessary before another formal measurement.

## Final-source CNAME diagnosis (#175)

**`release_qualified: false`.** The retained November 16, 2026 failure and the
[static and local diagnostic results](../dev-docs/milestones/v0.2/final-acceptance.md#local-zone-hot-path-diagnosis-175)
are distinct. The original reference's CNAME p99 went from 1,890 ns at
calibration (5.5% headroom) to 2,187 ns in the failed run under the unchanged
2,000 ns maximum. The local comparison did not isolate a source latency cost.

From the repository root, reproduce the retained ratios without timing:

```sh
evidence=dev-docs/milestones/v0.2/evidence/2026-11-16-final-source
vectors="$evidence/performance-corrected-metric-vectors.csv"
python3 -I hack/bench-evidence-compare.py \
  "$evidence/historical-calibration-inputs.tar.gz" "$vectors" --limits bench/limits.txt
python3 -I hack/bench-evidence-compare.py \
  "$vectors" "$vectors" --after-role candidate
```

The first command compares the same source across recordings, and the second
compares reference and candidate within the final recording. The tool copies
only the historical `blocking-zone.txt`, `cache.txt` and `upstream.txt` reports
into a fresh temporary directory, removes it when done and writes JSON to
stdout. It strips the native `-6` suffix, selects a CSV role before joining on
`(report, name, unit)`, and divides medians, not individual samples. The
`summary.ns_time` field covers only `ns/op`, `p50-ns` and `p99-ns`; zero reference
medians have no defined ratio. The matched count and per-metric vectors include
other units. `--limits` reads the CNAME p99 ceiling for descriptive headroom
only. This is not a provenance verifier, gate, or replacement recording.

The local comparison used a temporary detached worktree for `0cc9565e` and
head `61916ce5`. In each tree, use Go 1.27.4 with `CGO_ENABLED=0` to compile
`go test -c -o "$binary" ./internal/zone` and collect compiler diagnostics with
`go build -gcflags=-m ./internal/zone`. Compare `go tool nm -size "$binary"`
and `go tool objdump -s 'zone\.\(\*Zone\)\.(resolve|answer)$' "$binary"`. The
local run used fresh processes in reference/head, head/reference,
reference/head order, with separate logs and CPU profiles:

```sh
GOMAXPROCS=6 GODEBUG=gctrace=1 "$binary" -test.run '^$' \
  -test.bench '^BenchmarkLookup100k$' -test.cpu 6 -test.benchmem \
  -test.benchtime 500ms -test.v -test.cpuprofile "$profile"
```

Record `nproc`, `/proc/loadavg` and `/proc/pressure/cpu` before and after each
process, and inspect the profiles with `go tool pprof`. The instrumentation,
short duration and changing load make these diagnostic observations, not
qualification timings or a valid candidate-regression verdict.

## Cluster tests in the homelab

Go benchmarks run on the fixed runner class above. Hosted functional coverage
of a multi-node install already exists: the `kind-metallb` job
(`.github/workflows/kind.yml`, `hack/kind-e2e.sh`) runs one control-plane node
and three worker nodes (`hack/kind/cluster.yaml`) as containers on one runner
VM. It installs both charts with three replicas and MetalLB, then checks DNS
through the LoadBalancer and a rolling restart under load.

The homelab test cluster is for scoped diagnostic and functional runs whose
pods run on homelab Kubernetes workers instead of kind containers, such as a
load run through a Service against a `dev-image` build. It is a vCluster with
its own API server, etcd and `kudurru.io` CRDs, separate from the objects of
the production homelab install. Its workers also run production workloads.
The vCluster's logical isolation and scheduling do not prove physical host
placement, and they do not make its numbers comparable with other hosts.

Its runs are not counted. They are not release qualification, they do not
substitute for cross-host or fixed-runner timing (#143 remains open), and they
do not change any baseline, limit, calibration or evidence in this directory.
Compare a result only with other runs in the same cluster.

The cluster is defined in the private `homelab-docker-k8s` repository, in
`kubernetes/apps/kudurru-test/`. That directory's README is the runbook,
including access and placement. Run the helper from the root of a
`homelab-docker-k8s` checkout. `--src` names a separate Kudurru checkout, not
the homelab one; `/path/to/kudurru` and `dev-123` are examples:

```sh
kubernetes/apps/kudurru-test/kudurru.sh install --src /path/to/kudurru --tag dev-123
kubernetes/apps/kudurru-test/kudurru.sh dnsperf cached   # or: miss
```

`install` installs `deploy/helm/kudurru-crds` and `deploy/helm/kudurru` from
the Kudurru checkout named by `--src`. Check out the commit that matches the
selected image, so the charts and the image come from the same source. The
`dev-<PR>` tag comes from
`.github/workflows/dev-image.yml`, which builds it only for pull requests from
this repository that are not from Dependabot and that change a file outside
`docs/`, `dev-docs/`, `docs-site/`, `website/`, `branding/` and
`license-server/` (`hack/ci-changes.sh code`). Each push to the pull request
overwrites the tag; `--digest sha256:...` selects a fixed image instead. Keep
the image digests that `status` and `dnsperf` print with each result, not the
tag. Before a diagnosis depends on separate physical hosts, check where the
replicas and the load client actually ran: the printed node names, and the
host behind each worker.
