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
The original cache/strategy ceilings below are now calibrated; their candidate
failures, socket/persistence calibration and final comparison remain open. Local sandbox measurements, matching CPU
text or merely passing this label to the script cannot close the hosted gate.

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
blocking/zone absolute limits plus the independently calibrated cache/strategy
ceilings below. UDP and TLS query throughput (`q/s`) is higher-is-better; loss
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
remaining socket/persistence calibrations, external acceptance or other release gates.

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
PR) runs the immutable original reference and exact candidate sequentially in
one VM. It uses the existing collectors: twenty original-reference commands,
forty candidate commands, and their original shape/absolute gates. Both complete
source manifests and every numeric process receipt are retained. No failed
sample is retried, replaced, or selected for its CPU or outcome.

Only a complete reference with successful native commands and all three gates
can enter the three original-scope comparisons. A failed reference still permits
the independent candidate recording, but refuses comparison and fails the paired
result. The original Go gate retains five samples, six processors, strict CPU
matching, the 20% threshold, noise floors and absolute limits. Source changes,
resource stops, residual descendants and altered artifacts stop remaining work.
Candidate socket/persistence scopes stay calibration evidence;
`release_qualified` remains false pending their absolute calibration, resolution
of measured failures and all other release requirements.
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
its required gates. The collector pins all three limit-file identities, applies
the new limits to recording and comparison, and rejects a baseline with different
limits. Later cache Growth cases remain new; no old baseline is invented for them.

Applying the new ceilings to retained candidate run 42514599389 fails five cache
metrics (parallel hit, insertion bytes, three memory metrics) and three strategy
latencies. Its CPU also differs from the historical reference, so these absolute
observations are not a strict relative comparison. The failures remain open;
limits must not be raised to clear them. Socket/persistence calibration, the
paired original comparison and final-source release qualification remain open.


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
