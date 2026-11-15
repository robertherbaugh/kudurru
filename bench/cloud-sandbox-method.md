# Cloud-sandbox host amendment — owner accepted

The accepted shared homelab recording remains failed. Its immutable historical
zone lookup p99 medians exceed the unchanged 2,000-ns fixed product requirement.
No hardware profile or candidate permission can be derived from that attempt.

The accepted [method at fef3a1fd](https://github.com/robertherbaugh/kudurru/blob/fef3a1fde4cf8b847ba833d9e1f0710e7ec3f998/bench/dedicated-host-method.md)
states: "A failed attempt remains failed; another attempt requires a diagnosed
fix or separately reviewed method change." The owner accepted this amendment on November 15, 2026: "Approve the disclosed
sandbox method." This accepts the placement and one historical-only calibration;
actual values and candidate timing still require separate review. It is not a diagnosed fix or
a promise that another host will pass. The current sandbox's earlier diagnostic
measurements remain disclosed diagnostics and cannot replace this calibration.

## Specific placement proposed

Use this existing Vercel cloud sandbox, rather than creating another Proxmox VM.
It exposes eight x86-64 vCPUs (four virtual cores, two threads per core), Intel
Xeon at 2.50 GHz, family 6/model 85/stepping 7, KVM. Its guest affinity is recorded
in `sandbox-host-pending.json`, along with machine-ID hash, boot ID, kernel,
virtual CPU topology, memory, storage/cgroup bounds and Go environment.

Physical host identity, physical CPU pinning, SMT placement and competing tenant
workloads are not exposed. The proposed amendment explicitly accepts those
unknowns and physical sharing. It replaces the method's verified fixed physical
host and hypervisor pinning requirements for this named VM only. Guest identity
and resource settings must stay fixed through both calibration and qualification;
any detected VM/boot/affinity/cgroup/toolchain/storage change still holds the run.
The execution cgroup is `/conductor/workload`, stable across this sandbox's shell
commands. It is not a physical CPU reservation. All unrelated local builds/tests,
including the currently active isolated security race suite, must finish before
reviewed identity capture and timing. No production service is touched.

## Unchanged measurement and acceptance rules

After actual approval of this amendment and host record, predeclare exactly one
historical calibration with the collector at `fef3a1fd`. Use clean checkouts of
original `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2` and later
`9cc2ae0423070fc514c94a25ab5b92e3816889e7`. Retain all 40 native commands,
five fresh processes per scope, six processors, Go 1.27.4, CGO disabled, original
corpora/durations, all process/source/artifact guards, disk/output/time bounds,
and the exact fixed product rules. In particular, the 2-microsecond lookup p99,
allocation limits, 150-MB memory and original five-second compile bounds remain.

The collector's profile label is `shared-cloud-sandbox-v1` for this placement.
That metadata distinguishes it from `dedicated-homelab-v1`; the collection,
validation and limit-derivation implementation otherwise remains at `fef3a1fd`.

Every failure remains retained. No new attempt, sample replacement, quieter
host selection or baseline refresh is authorized after another failure.
Only an eligible historical calibration may propose the separate hardware
profiles using the unchanged 20%/noise-floor, throughput and zero-loss formulas.
Review of actual vectors, exact manifest and proposed limits remains a separate
required owner decision before any candidate timing. The final 60-command
interleaved qualification and reference eligibility requirements remain intact.
No passing legacy hardware ceiling is inferred, and no failed recording is
converted into a pass. Performance remains release-blocking until final review
and passing qualification; this amendment alone would not accept v0.2.0.

## Recorded decision

The owner accepts this disclosed sandbox placement and protocol amendment for
one fresh historical-only recording. The exact reviewed proposal and actual
owner decision are retained with the recording. Before timing, capture the
accepted guest identity after unrelated work finishes. No calibration limit,
candidate timing or release acceptance is supplied by this decision.

## Restarted sandbox proposal (pending review)

The follow-up workspace has a different VM/boot/CPU identity. The historical
acceptance above applies only to its named host. A proposed extension lets
`hack/bench-dedicated.py qualify --host-transition-approval PATH` accept a new
host only when a separate human-reviewed record binds the previous host hash,
new host hash, immutable calibration manifest and original calibration approval.
The new host is still guarded around every process, as are the calibration,
approval and source files. Without that record the same-host rejection remains.

No limits, workloads, process counts, reference eligibility, 20% relative gate
or failure-retention rules change. The fresh original/candidate measurements
must run on the same newly approved host, after unrelated builds/tests finish.
The proposal does not grant approval or convert the earlier runtime pass into
final-source qualification. The actual owner response must be recorded before
candidate timing; until then performance remains pending.
