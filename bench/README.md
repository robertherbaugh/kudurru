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

The workflow records measurements; a successful job is not release qualification.
New cache/strategy absolute limits, baseline calibration and final comparison
still need review on that actual runner. Local sandbox measurements, matching CPU
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
blocking/zone absolute limits. Cache/strategy absolute limits are not yet
calibrated. UDP and TLS query throughput (`q/s`) is higher-is-better; loss
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
remaining absolute calibrations, external acceptance or other release gates.
