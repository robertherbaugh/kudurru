# v0.2.0 follow-up acceptance

PR [#54](https://github.com/robertherbaugh/kudurru/pull/54) merged as
`04ff78aa` on November 15, 2026. That integration milestone does not publish
v0.2.0 or accept every feature package. The release remains **on hold** until
the open gates below have evidence and the required review decision. No release
tag, production deployment or publication is authorized by this report.

This report reconciles the package snapshot at `7663c957` with the later
[acceptance audit](followup-audit.md#final-integration-resumed-november-15-2026-d397-d399-d405d409),
[local release report](local-release-acceptance.md) and current implementation.
Historical failed runs remain failed. A later source pass supersedes only the
specific finding it exercises, not a package's complete acceptance criteria.

## Current work and gates

| Gate | Current evidence and remaining requirement |
|---|---|
| TSIG observers | Thirty race repetitions reproduced both historical failures: the expected credential/deadline errors and committed image were correct, while the publisher notification channel closed. The fixture issued a redundant Wake and included an unrelated retrying provider. The follow-up removes those notification sources only; expiry, original cancellation/deadline, quiet channels, publication, writer eligibility, audit and durable-image assertions remain. Final repeated results and hosted CI are recorded below. |
| #94 | Implemented per-group `cache: true`, default off for non-default groups. Default group caching and the global cache switch retain their existing precedence. The route plan is bound before lookup and partitions by group, routing and trust configuration. Covers conditional suffixes, forward-zone/Policy hints, TTL bounds/countdown, stale/prefetch, reload flushes and cache inspection. The dashboard saves the toggle immediately. Focused Go race suites, dashboard type checking, 40 upstream tests, lint and bundle build pass. |
| Packaged upgrade | Released v0.1.0 Linux archive and RPM downloaded and checked against published SHA256 checksums. The existing upgrade test now takes a stopped-volume backup and restores it under the released old binary, checking DNS, credentials, schemas and history. Candidate package execution and exact hashes/results are recorded below. |
| Performance | Earlier approval and pass bind runtime `5e19a32b`, not this source. Calibration manifest `3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266` remains immutable. This sandbox has a new VM/boot/CPU identity. Reusing its limits requires a separately reviewed host transition, a new guarded host record and fresh original-reference/candidate interleaving. The host amendment is pending; no final-source performance pass is claimed. |
| Hosted checks | PR #54's last reported source run `43770178234` failed the two observer tests and `ci-ok`; its other 23 jobs passed. Required checks for this follow-up must pass on its own head. |
| Manual UI and production license | Earlier HTTP/browser and synthetic-license proofs retain their exact scope. They do not establish human visual approval or production entitlement issuance/signing. Final-artifact browser/deployment checks below have their own scope. Remaining owner checks are tracked in [#133](https://github.com/robertherbaugh/kudurru/issues/133). |
| Review and publication | Required CI/security/review checks govern merge. Publishing v0.2.0 is a separate release-branch milestone. |

## Historical findings reconciled

These entries are closed as allegations of missing implementation or missing
specific proof; residual acceptance work stays in the next table.

| Package | Superseded historical statement | Current evidence |
|---|---|---|
| ZONE / HA / UPDATE | Command recapture and starvation fixes were reverted; follower applies are always cut by their own pull; UPDATE receipts impose a permanent 256-commit limit | D399, D405–D407, D409 and the final-integration audit identify the integrated fixes and mounted/process proofs. Receipts expire under the original signature deadline and bound simultaneously live receipts. Original lifetime and post-admission failure rules remain. |
| DNS | Managed-root writer ordering remains unexplained | D411 records the elected startup ordering correction and preserved acquisition/readiness assertions. Its runtime and final performance gates remain distinct. |
| RPZ | Kubernetes startup can capture empty policy and forward a listed name during restart | D410 captures policy/configuration coherently and rejects an unsynced/missing index; deterministic negative controls and mounted restart race checks are recorded. |
| IDENTITY / INTEGRATIONS | The inventory regression and observation scheduling have no integrated fix | D397/D408 and final integration record the lease MAC gate, start-paced observations, status refresh and post-publication integration startup, with 20 mounted race runs. Sustained CPU starvation remains a finite-budget limitation. |
| IAM | Roughly 4,000 revocations exhaust permanent history; TLS and serving-follower revocation proofs are absent | D413 replaces permanent-history exhaustion with checkpoint compaction. D414 and [IAM live acceptance](iam-live-acceptance.md) supply mounted TLS, original credential revocation, follower and live Authentik proofs; the later local report adds actual member-mTLS process replication. Do not reopen the obsolete 4,000-cycle limit. |
| Dashboard / OPS | Views, inventory, IAM administration, RPZ and integration pages are missing; Lists polls every pending feed at 3 seconds | DASH-OPEN2-FINISH is integrated; RecordEditor retains served-view namespaces and RPZ polling uses `rpzPollInterval`. Later audit evidence records full dashboard tests. Human visual acceptance stays separate. |
| Build / OPS | No full release snapshot or green source CI has ever completed | The local report records all 20 release steps, four platforms, five archives, six packages and both local images on `5e19a32b`; CI `43745021235` passed on that exact runtime. Neither result transfers automatically to this follow-up. |

The primary compiler checks dynamic ownership before adding endpoints
(`internal/primaryzone/compiler.go`), including view endpoints; the editor keeps
`initial.namespace` for served view rows (`RecordEditor.tsx`). Those historical
claims are no longer treated as confirmed current defects. Namespace, ownership,
transfer privacy and driver differences must still retain their existing tests.

## Remaining package findings

Every link below is a concrete follow-up with acceptance criteria. Filing an
issue does not defer a v0.2 requirement or accept a limitation. The owner must
resolve or explicitly disposition the applicable gates before publication.
Historical low-severity notes not independently reproduced retain their original
status in the audit, rather than becoming invented current failures.

| Package / remaining scope | Follow-up |
|---|---|
| ZONE: enabled external-source RBAC failures and caps; TSIG CLI on Kubernetes | [#123](https://github.com/robertherbaugh/kudurru/issues/123), [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| ZONE / DNS / EDGE: hosted pinned interop, signed SOA-only IXFR without AA, dnsdist 2.1.3 empty-FIN limitation | [#127](https://github.com/robertherbaugh/kudurru/issues/127) |
| UPDATE / HA: bounded failures after admission, long apply partial outcomes, member retirement and D395 ordinary-answer grace decision | [#129](https://github.com/robertherbaugh/kudurru/issues/129); D409 remains implemented |
| DNS: final performance qualification; managed-anchor residual metric/refusal/abandoned-term coverage | Performance gate above; historical F28 coverage notes remain open pending a current proof |
| RPZ / OPS: explanation projection, offline signed-feed restart, compile window and multi-port CoreDNS loss reporting | [#130](https://github.com/robertherbaugh/kudurru/issues/130) |
| INTEGRATIONS: whole-image capture/cleanup scale and original time bounds | [#124](https://github.com/robertherbaugh/kudurru/issues/124) |
| INTEGRATIONS / IDENTITY: partial UniFi update/delete recovery, actual live providers and unexplained partial-create stall | [#125](https://github.com/robertherbaugh/kudurru/issues/125) |
| INTEGRATIONS: Kubernetes K3/K7, limits/refusals, credential and lease-loss qualification | [#126](https://github.com/robertherbaugh/kudurru/issues/126) |
| INTEGRATIONS: webhook retention, mixed-version event cursor, budget/audit coverage and deferred event topics | [#132](https://github.com/robertherbaugh/kudurru/issues/132) |
| IAM: documented Kubernetes workflow refusals, restart-bound settings; remaining human/production-license approval | [#126](https://github.com/robertherbaugh/kudurru/issues/126), [#133](https://github.com/robertherbaugh/kudurru/issues/133) |
| OBS: hour-boundary summaries; Events, tracing and live sink final-source qualification | [#122](https://github.com/robertherbaugh/kudurru/issues/122), [#131](https://github.com/robertherbaugh/kudurru/issues/131) |
| EDGE: restore-hold continuity and temporarily unreachable backend behavior | [#128](https://github.com/robertherbaugh/kudurru/issues/128) |
| OPS: manual UI, production entitlement/branding, package/deployment and release protection checks | [#133](https://github.com/robertherbaugh/kudurru/issues/133) |

## Explicit future acceptance

AD/Windows production acceptance remains future work under **D353**. The
isolated GSS-TSIG probe is not production AD DNS replacement. Independent
**two-VM VIP/failover** acceptance remains future work under **D358**. Local
containers, a single VM, envtest and KIND/MetalLB cannot close that requirement.

## Final execution record

Execution results will be appended with exact source/artifact identities.
The uncommitted evidence directory is `.context/`; portable result summaries
belong here and in the follow-up PR. A fresh checkout cannot rely on local logs.
