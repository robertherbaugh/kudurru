# Final-source evidence: November 16, 2026

The qualification **failed**. The normal packaged candidate passed the issued
production-key checks. Neither result grants overall release acceptance.
Documentation PR [#144](https://github.com/robertherbaugh/kudurru/pull/144)
is stacked on source PR [#134](https://github.com/robertherbaugh/kudurru/pull/134) at
`a22255a2bc981a8fa12afcd2778267d7b681de1b`; this documentation stack records its
outcomes without changing that measured source. The
[acceptance report](../../final-acceptance.md) keeps all remaining gates explicit.

## Performance outcome

The owner-approved restarted-host sequence ran once from **00:43:41 to 01:10:32
UTC**, using Go 1.27.4, CGO disabled, six processors and five fresh processes per
scope. All 60 native processes completed with exit zero. The source, guest
identity, host-transition decision, historical calibration and original approval
remained guarded throughout. Physical placement, pinning, SMT placement and
competing tenants were unavailable, as disclosed and approved.

All **ten absolute report gates failed**. The original reference is ineligible,
so all three relative comparisons were refused. There is no valid relative
candidate-regression verdict. The 20% gate and every fixed and calibrated limit
remain unchanged. [#143](https://github.com/robertherbaugh/kudurru/issues/143)
tracks this release-blocking result and the conditions for any future timing.

| Fixed product metric | Original reference | Candidate | Unchanged maximum |
|---|---:|---:|---:|
| `BenchmarkLookup100k/cname` p99 | 2,187 ns | 2,363 ns | 2,000 ns |

The remaining 40 matched fixed-product checks passed. Additional calibrated
hardware limits failed in reference cache/upstream and candidate
cache/upstream/UDP/DoT/QUIC/persistence reports. Examples: original-reference
fast/strict upstream p99 349 us versus 176.4 us; candidate six-socket UDP
148,866 q/s versus the minimum 187,963.2 q/s. These are observations; they do not
establish physical-host contention or a source regression as the cause.

| Retained record | Contents |
|---|---|
| [Verification result](performance-corrected-verification.json) | All 159 qualification files, 60 distinct native process receipts, exact order/commands, ten gate receipts, report concatenation, source archives (406 original / 5,352 candidate paths), unchanged input hashes and comparison refusal checked independently |
| [Raw qualification](qualification-recording.tar.gz) | Complete unmodified recording, process/gate logs and receipts, source maps, predeclared plan, execution timestamps/exit, console and verification scripts |
| [Five-value metric vectors](performance-corrected-metric-vectors.csv) | All 811 metric vectors and medians, including failing metrics |
| [Fixed product values](performance-corrected-fixed-product-metrics.json) | All 42 matched fixed-product vectors, medians, bounds and results |
| [Raw-file hash manifest](performance-corrected-evidence-manifest.json) | Independent SHA256 inventory of the 159 qualification files; manifest SHA256 `e61801285bef4b5477b6094ea6290c7b9f35e5e2f9ffe10984cd2aa100b3fb9c` |
| [Earlier source-correction hold](source-correction-hold.tar.gz) | Incomplete `7a642715` record: 20 successful native processes, then a source guard hold for diagnosed CI corrections; no qualification pass and no reused samples. Also retains the unmeasured `50006070` plan superseded by the narrowly classified read-timeout fix |
| [Immutable historical inputs](historical-calibration-inputs.tar.gz) | Exact calibration, original approval and historical host from `01271cef7338aaace70dbe7d3d53e759d2c66868`; calibration manifest SHA256 `3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266` |
| [Original replacement-host proposal](performance-host-proposal.json), [reviewed host](performance-host-reviewed.json), [accepted transition](performance-host-transition-approved.json) | The actual owner decision and bindings to both hosts, immutable calibration and its original approval |

The collector exits **1** for failed qualification. The independent checker
exits **0** because it verifies that exact failed outcome, not because performance
passed. `release_qualified` remains false everywhere. Archived scripts contain
original workspace paths and document this execution; they are not permission
to repeat measurements. No sample was replaced, no baseline refreshed, and no
limit relaxed. Earlier historical failures remain failed in their original
records at `01271cef` and the commits it references.

## Issued production key and artifacts

The owner supplied an issued key from the Mac. It was encrypted before transfer,
used privately on a disposable node and deleted with the temporary transfer
material afterward. The owner-created Mac copy was removed; the original file
in Downloads was retained. No key, signature, license ID, licensee or signing
material is included in this directory.

The **normal unmodified archive executable**, SHA256
`b2207adfdb849dab8f26ec2c0544d1a10a9b9fbb274b62c1e22ac9cf4ecb2cfe`,
trusted its embedded production root and reported active Enterprise, three
nodes and all eight licensed features. All eight checks in
[production-license-result.json](production-license-result.json) passed:
signed-claim agreement; included/excluded UI features; policy, audit and history
gates; Enterprise branding save/render; restart/session/DNS persistence;
minute-watcher rejection of a tampered signature; restoration of the original
valid entitlement; and Community/default-branding fallback after withdrawal,
while retaining the existing policy, session and DNS. The watcher used its normal
minute interval. [Cleanup](production-license-cleanup.json) and the
[check script](production-license-acceptance.ts) document scope without secrets.
The [browser capture](production-license-artifact.png) was visually inspected by
the agent; it is not human approval.

This verifies acceptance of an already-issued production signature. It does
not exercise the license-server issuance workflow, actual expiration/grace under
a production key, every licensed integration, or the full human UI matrix.
Those remain in [#133](https://github.com/robertherbaugh/kudurru/issues/133).

The candidate is an unsigned local `0.2.0-rc.1-SNAPSHOT-d1725c59` snapshot, not
published GA artifacts. [Artifact identities](final-artifacts-inspection.json)
and [runtime-source comparison](runtime-source-comparison.json) bind it to the
final measured source. [Packaged upgrade](packaged-upgrade.log) passed all nine
released-v0.1.0/candidate tests, including backup restoration under the old
binary; [RPM evidence](rpm-rehearsal-result.json) excludes maintainer scripts
and actual systemd lifecycle. The main report details the separate
synthetic-license audit API fixture.

## Source tests, retained failures and merge scope

The original observer failures and the first concurrent post-fix failure are
retained as `tsig-before.log` and `tsig-after.log`. The final idle
`tsig-final-quiet.log` passes 30 repetitions of every observer case. Cleanup
setup failures and the corrected 20-repeat pass are also retained.
`update-ledger-after.log` remains a failed local run: 1,097/1,100 NOERROR, ten
SERVFAIL, 16 EOF retries and three TCP read timeouts. Client and pipeline both
have five-second deadlines. The correction narrowly retries read timeouts along
with EOF and SERVFAIL within the same original three-minute change deadline;
all 1,100 real NOERROR, four workers, client timeout and original
replay/expiry/restart assertions remain required. This test checks ledger
capacity/correctness, not a guarantee that each first attempt answers.

Hosted command shard 4/4 passed that corrected full test. All **26 jobs** in
source run `43781940600` passed on `a22255a2`, including both original observer
shards, e2e and `ci-ok`; `security-ok` also passed. The JSON snapshots retain
that final source run/check state. Green CI
does not waive performance, code-owner review or release protection. The ruleset
mismatch remains [#140](https://github.com/robertherbaugh/kudurru/issues/140).

`manifest.json` hashes every other file in this evidence directory. No release,
production deployment or merge occurred. D353 AD/Windows and D358 independent
two-VM VIP/failover remain future acceptance.
