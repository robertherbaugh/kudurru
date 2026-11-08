# Benchmark evidence

`make bench` runs each blocking and zone sample in a fresh Go process, five
processes per scope by default. It preserves native failures in the combined
output and invokes `hack/benchgate` against the committed baseline and limits.
The gate requires exactly `BENCH_COUNT` values for every current metric, including
informational metrics. A partial sample cannot silently reduce a median's count.

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
every current benchmark suffix (no suffix means one processor). `-strict-cpu`
requires CPU headers and compares against the baseline when supplied. Matching
CPU text and processor count do not establish a named fixed runner class.

The committed baseline and limits cover blocking and zone lookup. New metrics
are reported separately; they do not acquire calibrated performance limits merely
by appearing in the output. Cache/strategy, socket and persistence qualification
and the fixed-class runner remain tracked in the v0.2 completion ledger. Preserve
failed runs when fixing code; do not replace samples or reset the baseline to
clear a regression.
