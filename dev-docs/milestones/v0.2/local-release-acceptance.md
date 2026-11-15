# Local release and process acceptance, November 15, 2026

Source: `5e19a32bfb4d62f8524218f0e1cec3df6806eaa0`.

The owner authorized disposable acceptance infrastructure and confirmed that
this cloud sandbox could host it. The checks used private temporary directories,
loopback ports and credentials generated for these runs. No production Kudurru
instance or homelab repository was changed.

## Release binary

A normal release build with the real dashboard bundle starts as
`kudurru-dev-test` with SQLite and verified HTTPS. The external client completes
administrator login, creates a local user and proves its original cookie is
refused after disable/delete. It creates a ServiceAccount, issues a token, then
proves the token is refused after account replacement and deletion. After an
actual process restart, both revoked credentials remain refused; the unrelated
administrator cookie and a separately retained ServiceAccount token still work.
The process stops cleanly and its private database, certificates and credentials
are removed. No TLS verification is bypassed.

Evidence: `.context/release-local-final-20261115/standalone.py`,
`standalone.log`, and `standalone-result.json`, including the binary hash and
source revision. The initial harness incorrectly attempted an unsupported
single-user GET; the corrected harness reads the documented user inventory.
Its initial failure is retained separately.

## Actual network replication

Two independently started local processes complete reviewed activation, offline
join and actual member-mTLS replication. Their public APIs use verified HTTPS.
The original local cookie, issued ServiceAccount token and administrator cookie
work on both processes before the user/account mutations. The primary disables
and deletes the local user, then replaces/disables and deletes the service
account. Actual network replication makes both processes reject the original
user cookie and token while preserving the unrelated administrator session.
The primary is then stopped; the follower is restarted and retains both refusals
and the administrator session without contacting a live primary. Both processes
stop cleanly and all private identities, invitations and databases are removed.

This supplements D414's direct replica-install proof with actual network pulls.
It uses the repository's existing `licensetest` build tag and synthetic ten-node
license, so it is not a test of production license issuance or a release binary's
license verification. The single-node check above uses the normal release build.
Both members run on one sandbox; independent-host failure and VIP behavior remain
outside this proof. Evidence: `network-iam.py`, `network-build.log`,
`network-iam.log`, and `network-iam-result.json` in the same local evidence folder.

## Live Authentik authorization-code flow

A separate disposable SQLite process connects to `auth.herbaughmanor.net` over
normally verified public TLS. A uniquely owned application, authorization-code
provider, password user and TOTP device are provisioned for this test. An
automated browser HTTP client completes the actual password and TOTP flow,
checks the authenticated Authentik username, and follows Kudurru's real OIDC
start/callback flow with its normal state, PKCE and nonce checks. The resulting
secure session cookie and newly issued personal API token both authenticate.
Deleting the API-created IdentityProvider rejects both original credentials,
including after an actual process restart; the unrelated administrator session
continues to work.

The harness observes the existing whole-second boundary between external-account
creation and token issuance. An initial immediate issuance returns 401 because
the stored token would predate the new account; that refusal is retained. The
successful run waits past this boundary before issuing once. Initial harness
errors adapting to Authentik's redirects, MFA and secure cookie name are also
retained, with verified cleanup. No product code changes or mutation replay are
used to obtain the passing run.

Cleanup removes the exact application, provider, user, TOTP factor and associated
session, verifies all six remaining counts are zero, then stops the process and
removes its private database, keys and credentials. Shared signing certificates,
flows and scope mappings are retained. Evidence: `authentik-browser.py`,
`authentik-browser.log`, `authentik-browser-result.json` and
`authentik-browser-cleanup.json` in the local evidence folder.

This uses the existing synthetic license fixture. It proves a live
authorization-code flow through an automated HTTP client; it does not claim
manual or visual browser UI acceptance or production license issuance. The
earlier live machine-token and trusted-TLS provider-double proofs remain
complementary.

The separate `authentik-network-browser.py` run also passes with two actual
primary/follower processes and member-mTLS replication. The original live-code
cookie and personal token authenticate on both members before provider deletion;
replication then makes both members reject both originals. With the primary
stopped, follower restart retains refusal and the unrelated administrator
session still works. Both processes and all private state are removed.

This preserves the cold-follower provider rule: its first historical entitlement
is initially refused, as recorded. Restarting the real primary while the follower
stays running establishes the next writer entitlement before external login.
The active pre/post credential checks establish revocation. Restart denial alone
cannot distinguish retained revocation floors from cold-start entitlement denial.
The follower intentionally hides login-provider choices in `/auth/status`; that
endpoint is not used as its provider-readiness check.

Evidence: `authentik-network-browser.log`, `authentik-network-browser-result.json`
and `authentik-network-browser-cleanup.json`. The all-attempt audit
`authentik-browser-all-attempts-cleanup.json` verifies zero applications,
providers, users and TOTP devices across both unique disposable prefixes.
Successful and failed attempts retain their separate cleanup receipts and logs.

## Release artifacts

All 20 local release steps pass on the exact source, including version and
generated-file drift, candidate manifests, real dashboard embedding, size,
single-target/full snapshots, live recursive DNS, managed-root bootstrap and
clean-tree cleanup. The normal bundled binary is 85,684,384 bytes against the
100,000,000-byte limit. Go 1.27.3, checksum-verified GoReleaser 2.18.3 and syft
1.54.1 are retained from the prior qualified tool installation.

The full snapshot takes 391 seconds including image checks. Independent
inspection verifies all 22 checksum entries, five archives, six deb/rpm/apk
packages and four cgo-free Linux/macOS amd64/arm64 binaries with the expected
source revision. Both locally loaded ko images report the expected snapshot
version and are removed. The snapshot name is tag-derived,
`0.1.0-rc.5-SNAPSHOT-5e19a32b`; separately staged candidate references use
`0.2.0-rc.1`. No release was published, signed, tagged or promoted.

The initial run passes 19 steps but refuses the full snapshot at its unchanged
14-GB admission guard after temporary decoded trace output consumes space.
That diagnostic text and additional rebuildable Go cache are removed; all
original traces, evidence and worktrees remain. The complete repeat retains
the default guard, starts with 17.19 GiB free, reaches 10.37 GiB free and finishes
with 12.31 GiB free, without a disk-floor stop. The failed admission is retained
separately from the successful complete repeat. This is a packaging retry after
a resource refusal, not a performance sample or a replacement of one.

The plain distroless snapshot bases retain their previously documented
informational volume-ownership probe failures; these are not counted as passing
volume checks. The separate current-source hosted edge workflow passes the
prepared release-base/image volume checks. Live DNS returns authenticated data,
refuses bogus DNSSEC with EDE 6, and answers with checking disabled: 18 outgoing
queries and zero timeouts.

Evidence: `full-retry/local-release/summary.txt`, `full-retry/run-result.json`,
`snapshot-inspection.log`, retained snapshot metadata/checksums, and sampled disk
receipts under the same ignored local evidence folder. These local artifacts
are not available from a fresh Git checkout. The earlier 2c2d7121 snapshot is
retained as predecessor evidence rather than used to qualify this source.

## Performance

Formal D412 run 43690014307 remains failed and release-blocking. Local tests do
not satisfy the selected hosted-runner protocol. No formal recording was
retried, no sample replaced, and no workload, baseline or limit changed.

Further analysis uses the existing hosted diagnostic 43745032589, without new
measurements. It follows individual exchange-worker goroutines rather than
dividing aggregate profile time by a query count. Each workload's exchange
count, after exactly 20 warmups, matches its logged benchmark iteration count;
worker state intervals conserve its lifetime. On traced fast/RTT-weighted,
reference and candidate mean worker lifetimes are 140.55/97.54 microseconds.
Mean runnable time is only 3.94/3.16 microseconds. The difference also spans
running, syscall and network-wait states. A Go worker's ready-queue delay alone
does not account for the gap. These are instrumented lifetimes, not the full
caller's latency, and do not identify OS scheduling/steal as the cause or prove
a product regression. Raw hosted traces remain unchanged. The extraction and
validated counts are retained in `trace-analysis/` beside the local evidence.

Grouping those same recorded exchanges by fake UDP listener also fails to
isolate one slow endpoint: reference fast/RTT listener means are
139.00/141.87/140.82 microseconds, compared with candidate
97.52/96.85/98.24 microseconds. Grouping by the worker's first Go processor
similarly leaves the difference across processor groups. Go processor IDs are
runtime scheduling slots, not measurements of physical CPU placement. These
observations narrow two hypotheses; they do not establish the cause.

A separate [phase diagnostic 43748833061](https://github.com/robertherbaugh/kudurru/actions/runs/43748833061)
runs once on an isolated helper branch, leaving PR #54's source CI intact. Its
[collector and declared sequence](https://github.com/robertherbaugh/kudurru/blob/bdd81cce73118a3def949964b06fbd0ad4bb77fb/hack/bench-phase-diagnostic.py)
use overlays to add trace regions and CPU receipts around each benchmark body,
after the 20 warmups and before sorting or cleanup. Compilation is outside those
receipts. The original reference and runtime sources remain unchanged. Both
overlays build and emit valid single-exchange smoke receipts locally; actionlint
and all 21 existing collector control tests pass before the helper push.

On EPYC 9454, Go 1.27.4 and six processors, all eight declared native processes
complete; 48 artifact hashes and the 406/5,346-path source manifests verify.
Every phase iteration count matches its native benchmark log. The measurements
are instrumented diagnostics, with no qualification gates or baseline read:

| Process | Phase wall, microseconds/query | Process user CPU | Process system CPU | Guest steal |
| --- | ---: | ---: | ---: | ---: |
| Reference strict 1 | 116.35 | 75.39 | 86.23 | 0.55% |
| Candidate strict 1 | 102.54 | 64.28 | 78.35 | 0.63% |
| Reference RTT 1 | 106.47 | 67.04 | 79.23 | 0.81% |
| Candidate RTT 1 | 110.00 | 67.67 | 80.06 | 3.66% |
| Candidate strict 2 | 97.23 | 62.18 | 72.03 | 0.84% |
| Reference strict 2 | 93.31 | 54.96 | 72.04 | 1.57% |
| Candidate RTT 2 | 103.60 | 65.52 | 79.40 | 1.45% |
| Reference RTT 2 | 109.60 | 67.29 | 80.24 | 2.10% |

CPU columns are also microseconds/query and include parallel client, fake-server
and tracing threads, so their sum can exceed wall time. Guest steal is a VM-wide
counter over the workload interval, not a per-query attribution. Reference strict
changes from 116 to 93 microseconds while charged CPU also falls; these data do
not establish CPU steal alone as the explanation. This new VM's observations
cannot explain or replace failed D412. The raw artifact remains unchanged under
`hosted-phase-43748833061/` in the same ignored evidence directory; analysis is
outside it. No diagnostic was selected for its CPU or result.

Additional extraction from all eight retained phase traces matches each exact
iteration count. The exchange worker spends 21.78–25.73 microseconds/query in
Go's syscall state. Each query has one traced write, close, connect and socket
option call, and approximately two reads, on both sources. Syscall-state time
includes any OS scheduling during the call; it is not kernel CPU time and
excludes other threads. This does not identify a changed call count or a causal
product fix. Scripts and receipts are retained in `trace-analysis/`.

Diagnostic evaluation of D412's original five-process reports, with the original
comparison rules, finds no relative regression: blocking/zone and cache checks
pass, while upstream still fails its seven candidate absolute limits. Report
hashes agree before and after evaluation. The historical reference failed its
absolute gate, so these calculations remain ineligible as formal comparisons
and cannot qualify the release. No new timing or formal recording occurs.
Evidence is under `vector-analysis/`; its initial exploration without the rules
is retained separately and is not credited because it included the informational
p99.9 metric that the original rules ignore.

The owner authorized the dedicated-host method and then accepted VM 20027's
pinned CPUs with disclosed physical sharing. The amended method and collector
are isolated at [`fef3a1fd`](https://github.com/robertherbaugh/kudurru/commit/fef3a1fd).
All 31 controls pass. The owner decision accepts placement and historical
calibration; unseen limits and candidate measurement still require separate
review. Other guests retain unrestricted affinity. Existing VMs, production
Kudurru and cluster networking are not changed.

One historical-only calibration on `kudurru-dev-test` / `hm-pve02` runs from
20:08:00 to 20:26:31 UTC. Go 1.27.4, six processors, five fresh processes per
scope, original corpora/durations and all fixed product bounds are preserved.
All 40 native commands succeed with no resource stop or unreaped descendants.
Six shape gates pass; the blocking/zone product gate fails these immutable
historical-reference metrics:

| Zone lookup p99 | Five values, ns | Median, ns | Fixed maximum, ns |
| --- | --- | ---: | ---: |
| Hit | 2626, 1213, 2405, 3508, 2430 | 2430 | 2000 |
| CNAME | 2998, 4194, 4761, 4907, 10335 | 4761 | 2000 |
| Wildcard | 2609, 2586, 9103, 2634, 8443 | 2634 | 2000 |

The other fixed product metrics pass. The six passing shape gates do not claim
that old hardware ceilings passed. Calibration is ineligible: no proposed limit
files, approval template or candidate timing are produced. Original failed D412
and this recording remain failed. No sample is replaced, measurement repeated,
or bound relaxed. These observations do not establish a cause of the latency
misses.

Independent verification checks all 114 calibration artifact hashes, 40 exact
command receipts with distinct process IDs, and source manifests against Git
archives: 406 original, 4,636 later and 5,348 control paths. Physical host and VM
configuration match before/after; guest and hypervisor contention counters and
service properties are retained. The calibration manifest SHA256 is
`5e1d12269c7875a980aa3330960a39418959382ff7fb280dc2ede185c50a3f0f`.
The [durable failed evidence and report](https://github.com/robertherbaugh/kudurru/blob/ca93be17c6ebe24fbdf52751eb562fd71c6a9c75/bench/calibration/homelab-shared-20261115-failed/REPORT.md)
are committed separately at `ca93be17`; local working evidence is under
`.context/release-shared-calibration-20261115/`. Private SSH material is outside
that evidence directory. The benchmark service is finished, with no candidate
measurement authorized by this failed calibration.

A causal implementation fix or a justified, reviewed measurement-method change
is still required before another formal recording under D383/D412. This report
does not accept or waive that blocker. Independent GitHub review is excluded
from the owner's requested work. AD/Windows and independent two-VM VIP acceptance
retain D353/D358 deferrals; the external Cloudflare docs build stays excluded.

## Hosted checks and acceptance scope

The current-source KIND/MetalLB workflow 43745021256 passes, along with the
companion docs, security, Compose, dev-image, edge, website, license-server and
AD-preview workflows. [Source CI 43745021235](https://github.com/robertherbaugh/kudurru/actions/runs/43745021235)
passes all 22 jobs on exact runtime source `5e19a32b`, including both Go race
shards, end-to-end acceptance and the final `ci-ok` job. Complete result metadata
and logs are retained in `source-ci-result.json` and `source-ci-final.log`. These scoped proofs do not
automatically accept every historical feature-package finding.

The documentation-head CI run 43754407458 separately times out
`TestIAMChurnCrossesFormerHistoryCapacity` in its security package at the
unchanged 30-minute bound. The identical runtime package passed in 1,501 seconds
in 43745021235; the failing stack shows active SQLite work, with no assertion
failure or race report. This is not a passing head check. The runner now isolates
the complete security package from concurrently executed packages while retaining
all 4,113 cycles, assertions, race instrumentation and its 30-minute timeout.
All seven runner controls pass. An isolated native race invocation is active;
hosted validation of that scheduling change remains pending. Runtime product
code is unchanged by this correction.
