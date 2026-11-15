# Dedicated-host performance method (prepared; acceptance pending)

The owner authorized preparing this method on November 15, 2026. No host,
isolation evidence or calibration values have been accepted. D383/D412 remains
release-blocking. The failed Ubicloud recordings and existing baseline, limits,
workloads and regression thresholds remain unchanged. A new profile applies
only after separate owner review; it does not turn the old failures into passes.

## Reserve and review the host

Use one disposable Linux amd64 VM, provisioned as `kudurru-dev-test`, with at
least six reserved processors, adequate memory and disk, and a fixed physical
host. Record the hypervisor, VM identifier, physical CPU topology and pinning,
sibling sharing, guest image, storage and evidence that competing workloads
cannot use the reserved CPUs during either measurement. A guest fingerprint
cannot establish physical isolation. Stop unrelated jobs and prevent migration,
ballooning, resource overcommit and automatic host maintenance. Do not use a
production Kudurru VM or the unrelated `pve-panel` business/customer pool.

Install Go 1.27.4, Git, Python 3 and `findmnt`; use `GOTOOLCHAIN=go1.27.4` and
`CGO_ENABLED=0` for every phase. In the exact runner checkout and execution
context, capture a pending host record:

```sh
python3 -B hack/bench-dedicated.py inspect-host --output /var/lib/kudurru-benchmark/host.json
```

Complete the host fields and obtain recorded owner review. `decision: accepted`,
`reviewed_by`, `reviewed_at` and `review_evidence` are a human attestation, not
an automated approval or cryptographic signature. Preserve the actual review
reference. The collector refuses pending records. It checks machine-ID hash,
boot ID, kernel, CPU topology/affinity, cgroup bounds, memory, storage and Go
settings before and after every bounded process. Reboot, migration, reservation
change or host drift holds the sequence and requires review. The host lock
serializes these collectors; it cannot reserve hypervisor resources by itself.

Predeclare the recording identifier, hardware and source identities before
timing. Keep evidence outside the historical checkouts and outside any runner
cleanup path. Do not replace samples, repeat for a favorable host, or discard
failures. A failed attempt remains failed; another attempt requires a diagnosed
fix or separately reviewed method change.

## Historical calibration, then review

Prepare clean, distinct checkouts at immutable original reference
`0cc9565e9a878f8223fc1cee5201eb7eca8aacd2` and later socket/persistence reference
`9cc2ae0423070fc514c94a25ab5b92e3816889e7`. Commit the collector before use. On
the reviewed host, record exactly 40 native processes: five fresh processes per
scope, original source for blocking/zone/cache/upstream, later source for
UDP/DoT/QUIC/persistence. Existing commands retain the original corpora and
one-second duration, except UDP's existing five seconds. Existing 30-minute
test bounds, 2 MiB output bound, process-group cleanup and 3/2 GiB disk floors
apply unchanged.

```sh
python3 -B hack/bench-dedicated.py calibrate \
  --host-record /var/lib/kudurru-benchmark/host.json \
  --reference /path/to/original --later-reference /path/to/later \
  --output /var/lib/kudurru-benchmark/v020/calibration
```

All native failures are retained. Complete reports must pass strict Go shape,
five-sample, six-processor, CPU and native-failure checks. Required historical
metric inventories are taken from the retained original baseline and calibration
provenance. The original `bench/limits.txt` product requirements remain fixed:
2-microsecond lookup p99, allocation bounds, 150 MB memory and five-second compile.
A failed native/shape/product check cannot establish a calibration.

Only a successful calibration produces separate `*-limits.txt` proposals for
cache/upstream/socket/persistence. Every original metric is required. Upper
bounds retain historical median plus max(20%, original noise floor: 25 ns/op or
64 B/op where applicable). Throughput/entry minima retain 80% of the median;
every loss/failure value must be zero. No candidate recording contributes a
value. Legacy hardware ceilings remain in the repository and their failures
remain recorded. The new calibration checks admissibility without pretending
those old hardware ceilings passed.

Review every raw vector, native receipt, source manifest, host/isolation record,
command, fixed-product gate and proposed bound. The manifest hashes all retained
calibration files. Create a pending review template:

```sh
python3 -B hack/bench-dedicated.py review-template \
  --calibration /var/lib/kudurru-benchmark/v020/calibration \
  --output /var/lib/kudurru-benchmark/v020/approval.json
```

Obtain actual owner acceptance of the specific manifest digest and limits before
changing this pending template. Publish the reviewed calibration and all vectors
in the existing `bench/calibration/` format in a separate reviewed change. The
collector never grants this approval. Preparation authorization does not approve
unseen hardware or limits. Calibration always retains `release_qualified: false`.

## Exact-source paired qualification

After review, use the same VM, boot, reservation, toolchain and hardware profile:

```sh
python3 -B hack/bench-dedicated.py qualify \
  --host-record /var/lib/kudurru-benchmark/host.json --reference /path/to/original \
  --calibration /var/lib/kudurru-benchmark/v020/calibration \
  --approval /var/lib/kudurru-benchmark/v020/approval.json \
  --output /var/lib/kudurru-benchmark/v020/qualification
```

This reuses the D412 engine's complete 60-command interleaved sequence, original
20% relative threshold, noise floors, required reference eligibility, fixed
product bounds and all process/source/artifact guards. The only substituted
limits are the separately reviewed hardware profile; original files are never
edited. The original 20-reference/40-candidate command inventory stays intact.
Candidate socket/persistence scopes retain absolute checks against the historical
profile; no additional relative baseline is invented for them. A failed
reference refuses comparison. Any failed native, absolute or relative gate
fails the recording. Changed host, source, approval or calibration holds the
remaining work. Evidence lives under `qualification/paired/` plus the bound
review record. Even a performance pass does not accept other release gates;
`release_qualified` remains false.

The manual-only `benchmarks-dedicated.yml` workflow is prepared for a runner
explicitly labeled `kudurru-benchmark`. It has no PR trigger and is not registered
or dispatched by this preparation. Reserve `/var/lib/kudurru-benchmark/` for
non-secret evidence; the workflow uploads that entire directory, including
failures, for 90 days. Keep a durable archive separately. Register the runner
only on the reviewed VM and pre-create the recording directory. Before this
workflow reaches the default branch, run the same CLI on the reviewed host;
do not add a push trigger merely to obtain a timing run.
