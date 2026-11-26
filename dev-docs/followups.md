# Follow-ups

The [current v0.2.0 reconciliation](milestones/v0.2/final-acceptance.md)
supersedes specific historical allegations below and links remaining work to
issues #122–#133 and #135–#140. Original entries retain their historical evidence; issue
creation does not waive release requirements.

Package lifecycle (#167, part of #133): the optional `packages` workflow
exercises maintainer scripts and systemd on an Ubuntu VM and in Rocky Linux.
The [verified closeout](milestones/v0.2/final-acceptance.md#verified-issue-closeout-november-21-2026)
records green hosted `package-lifecycle` results and the checksums for both
snapshot formats.
The check remains optional. The November 16, 2026 RPM rehearsal did not run
maintainer scripts or systemd, and snapshot results do not replace evidence
for the published packages.
See [the release checklist](release-checklist.md#3-checks-before-ga-ci-and-the-owners-hardware).

For #133, use the [manual release acceptance runbook](milestones/v0.2/manual-acceptance.md)
for open final-artifact human UI approval, authorized production issuance,
and expiry/grace evidence. The November 20, 2026 owner decision prefers the
issued-key disposable VM run and allows the labeled synthetic fallback for
expiry/grace only.

Open items that are not on the milestone exit criteria they came from. Each
names its source (spike or milestone report) and the milestone where it should
land. Close an item by deleting its row and citing the commit. Items that need
the project owner's hardware (two VMs, a real dnsdist driven with kdig) are
collected in the "Needs the project owner's hardware" section and in
dev-docs/release-checklist.md; Docker and kind run on GitHub's runners.

For v0.2, use the [item-by-item evidence audit](milestones/v0.2/followup-audit.md)
and [implementation ledger](milestones/v0.2/gaps.md) alongside this original
requirements list. They split bundled rows, identify already implemented work,
and record subsequent closures with validation; older descriptions here are
not a current feature-completeness claim. Requirements and release gates remain
in scope unless explicitly retargeted by the owner.

Closed rows from this pass: remote node health (`49f9e8b`), central Grafana
panels (`60a3af1`), nested settings help (`d37f4ad`), audit verification API/CLI
(`3df864c`, `a097836`), and optional webhook authentication (`5fc97d7`).
The evidence audit preserves their original requirements and limits.

Closed rows of the release documentation pass (G25-DOCS, November 13, 2026),
deleted because the follow-up's own item is done: the
[completion plan inventory](milestones/v0.2/completion-plan.md#complete-subrequirement-inventory)
marks their F rows integrated, implemented or complete, or (F21c, F60b, F60c,
F67) active with the part this file tracked done and only package or release
gates left. Each names its F row and the commit or decision that did the work:
cold-start cluster identity (F01, `1bf8586`); SQLite audit attribution and redaction, epoch fencing and internal kinds (F02a, F02b, F38 `aa3784c`; F02c `5079fa7`; F02d `524d16a8`, `f044fcd3`); watch resume from a resourceVersion (F03, `cf35c627`); the shared rejection table (F04, `d4075f5`); parsed record reuse (F05, `eb20ac7`); per-zone NOTIFY key and source (F06a, F06b, `c80c6f2`, `46d7641`); the `doctor` self-NOTIFY round trip (F08, D261, D331); per-integration Leases (F09a, `7d653e7`); inline object reload and the `Policy/default` fallback (F10a `71c0393`, F10b `0ac2fefb`); current-hour stats beyond 100,000 names (F16, `8b98f41`); `$important` and `$badfilter` (F17, `292fdb9`); control settings and the extra hostname sources (F18a `50773f9`, F18b `83c1096`, F18c `380f43f`); the replicated blocking toggle (F19a; F19b, D301); dnsmasq import, cache inspect and the cache and strategy limits (F21a, F21b; F21c paired run 42554034011 on `9963dec2`); padding, Alt-Svc, listener limits and DoH path IDs (F22a–F22d); the public catalog data repository (F23, D333); rate-limit exemptions per ClientGroup (F24b, `3234ad4`; F24a `661174c`); RFC 8198 (F27, D296); negative trust anchors (F29a `9d7bd79`, F29b `ddc5f2d`); validation of forwarded answers (F30, `9d7bd79`); DNS cookies (F31, `668203c`); durable node settings (F32, `ffcf20e`, `bd47907`); scoped permissions in `/auth/me` (F34, `b8579a8`; zone groups D385); the direct DNSEndpoint source and the webhook sidecar (F35a D302/D303, F35b D288); object lists from the live snapshot (F36, `92bf51b`); the generated Go client under the CLI (F37, `a106708`); dedicated audit columns (F38, `aa3784c`); remote edge cache invalidation (F39b, D283); host-only edge and VIP keys and VIP audit events (F40a `52ec29d`, F40b `380458f`); ServiceProvider owner and chart annotations (F41a `1c934e7`, `9990ede`; F41b `c2b50011`, D272); per-VRID HMAC references (F42, `376d63d`); console diff-apply and certificate reload (F43a `e60001f`, `88ca477`; F43b `e60001f`; F43c D275); durable binding revocation (F44a `15acbd8`; F44b `c44b67d`, D364); stored OIDC identities and their administration (F45a `4aca7c2`, F45b D289); bounded projected ServiceAccount tokens (F47, `101b614`); `autoProvision: never`, `syncGroups` and RP-initiated logout (F48a `4aca7c2`, F48b `2947b02`, `6f278bf`); ServiceAccount subject overlap (F50, D254); HTTPS-only downloads (F51, `a15480e`); the matched exclusion in dig answers (F52, `8879459`); exact ClientGroup scope of allow and deny lists (F53, `926b2a3`); regex override information and cache invalidation (F54a `66d74761`, D268; F54b D283); the per-digest entries index (F55, `dbfd2963`, D267); self-hosted fonts (F56, `13c0c9e`); the record-delete dialog and query export (F58a `2f362c65`; F58b `13dceddd`, D258); kubeconfig contexts (F59, D285); the §3.5 verbs: cluster commands (F60b, D345, D347, D354, D355), the `integrations` CLI (F60c, D291, D295, D298, D367; the dashboard page stays open below), zone options (F60d, `c9f14dc`), offline importers (F60a, `32f9e81`, `f01d4db`; CoreDNS D392); browser and device CLI login (F61a `4fddbce`, F61b `b09fc77`); CLI conformance and the §10 contracts (F62a `6938fbb`, F62b `c439532`); `cluster.role: static` (F65, D300); separate querylog and local paths (F66, `b3f6986`, `5048c61`); E2E-2 static SQLite membership (F67, D300, D349); retired-node central reads (F69, `a4e38ce`); central dictionary pruning (F70, `eef61bd`, `b1a29e8`, `a7304d8`); partial-hour rollups of stopped sources (F71, `123148b`, `f8fbb85`); AuditLog diagnostics (F72a, F72b, `e77d025`); the AuditLog verification-key lifecycle (F73, `d3604c1`). Their final release gates (G21–G24) and the
package verdicts in the [acceptance table](milestones/v0.2/gaps.md#feature-package-acceptance-at-7663c957-november-14-2026)
still apply; deleting a row closes the follow-up, not the package. Rows whose
work is not complete stay below with the remaining part restated; none was
retargeted except F64c, whose remaining metric was already the original v1.0
subtarget.

## Zone, controller, objects

Partial SQLite view refusals (#171) are visible without changing the served
projection or the envelope format. Evidence: `TestCompilerSummarizesRefusedViewsInServedSets`,
`TestCompilerSummarizesSharedViewBudgetAcrossNamespaces`, the mounted SQLite
`TestRunServeSQLiteReadPartialViewRefusal`, and the namespaced editor test in
`dashboard/src/components/RecordViews.test.tsx`.
This does not change the SQLite takeover behavior, which is now documented in
[Zones and transfers](../docs/architecture/zones.md#client-group-views-split-horizon).

| Item | Source | Target |
|---|---|---|
| Stored TSIG keys: SQLite and Kubernetes management with hot reload (D292, D314, D365) and the `kudurru tsigkeys` CLI (D394) are integrated, and TSIG on DoH/DoQ/DoH3 is integrated (F07b, `a6bf8e1`). Open: `kudurru tsigkeys` has not run against a Kubernetes node, and F07a's final gates (G21–G24) | dev-docs/milestones/m2/xfr.md; F07a | v0.2 |
| DS at a delegation point where parent and child are both local routes to the child; needs a DS special case | zone report | v1.0 (DNSSEC) |
| Zone transfers across other commits: D419 keeps transfer answers current only across the NOTIFY acknowledgment, a primary zone's status projection and its receipt realignment. Any other commit (an unrelated object write such as a token or sign-in, the controller's or the list fetcher's status of other kinds) still withholds every transfer answer, refusals included, until the node publishes again (D395). Extending the transfer-neutral set needs a proof per shape that no transfer is computed from it | #236 (D419) | v0.3 |

## Active Directory DNS replacement (D222)

The [functional outline](active-directory-dns.md) is the scope and acceptance
matrix. These items are required for complete replacement in a single-domain
forest; a read-only import or permanent DC forwarder does not satisfy it.

| Item | Source | Target |
|---|---|---|
| Prove server-side Kerberos/GSS-TSIG and TKEY against real Windows DC, member, and DHCP registrations; SPN/key provisioning, rotation, edge/VIP routing, dependency licensing, and cgo-free build | D222, outline §§5, 10 | v0.2 feasibility gate |
| Design atomic RFC 2136 commits, principal/record ownership and AD permission translation, imported-record adoption, durable storage and replication of content/security/aging state, and fenced failover for each supported deployment mode | D222, outline §§4, 5, 7 | v0.2 |
| Discovery and import with metadata/blocker reports, shadow comparisons, final write handoff, source reconciliation, and rollback that retains post-cutover updates | D222, outline §§3, 4, 8 | v0.2 |
| Windows DHCP interoperability including enabled name protection, aging/scavenging, administration, diagnostics, and backup/restore | D222, outline §§6, 7 | v0.2 |
| Windows acceptance matrix with all DC DNS services stopped: domain/DC/member lifecycle, AD health, secure registrations, DHCP, permissions, aging, scale, outage/failover, and rollback; publish tested OS versions and deployment guarantees | D222, outline §9 | v0.2 release gate for claiming AD DNS replacement |

## Dataplane

| Item | Source | Target |
|---|---|---|
| DHCPv4 deployment acceptance with the target router/relay hardware and systemd/container packaging ([procedure](../docs/deploy/dhcp-cutover.md)). Disconnected stock-dhcpcd lifecycle, relay and multi-subnet tests pass; production LAN acceptance remains operator-specific | D171–D179 | v0.1.0 deployment acceptance |
| DHCPv6 protocol coverage and coordinated IPv6 ownership | D176; explicitly excluded by project owner from this DHCPv4 expansion | beyond this DHCPv4 pass |
| Previous-hour query-log counts (#122): `63004b9c` fixed live reads. `TestDirtyHourReadsFinerTiers`, `TestSummaryLateWritesIntoPreviousHour` (after a minute flush) and `TestClusterStatsMergeAtHourBoundary` (HH:00:00 and HH:00:30) fail with that read fix reverted. The restart gap is covered by `TestDirtyHourSurvivesRestart` (fails on `ce3ab6e2`) and `TestStoppedSourceHourBoundaryDirtyCursor`. Persisted dirty hours keep summary and client counts aligned with query facts until maintenance rebuilds them. | PR #75; `63004b9c`; #122 | Implemented; see #122 for verification |
| Lazy-DFA regex engine integrated (F20d, D293). Open: regex-1000 p99 stays adverse and the original relative benchmark gate exits 1; corpus and scale qualification stays in G23 | blocking spike; F20d | v0.2 |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | dev-docs/milestones/m2/xfr.md | later |
| Multi-socket DoT (F25a, `eeb68c2`) and CID-routed DoQ/DoH3 (F25b, D311) are integrated. Open: one local sample worsens HTTP3 by 12.54%; fixed-class performance and final release checks (G23) | hardening report; F25b | v0.2 |
| Settings hot reload: pseudonym identity (F26a), cache shard growth (F26b, `62990dd`), blocking refresh and degraded settings (F26c, `38153fa`), query-log sinks (F26d) and the five auth controls on complete SQLite (F26e, D290) reload without a restart. Open: on Kubernetes and for host inputs the auth controls stay restart-bound (`restartRequired` names them); privacy mode and method changes keep separate lifecycles | dev-docs/milestones/m5/burndown-server.md; F26e | v0.2 |

## Recursive resolver (D170)

| Item | Source | Target |
|---|---|---|
| Automatic root trust-anchor updates (RFC 5011): managed anchors and the F28 follow-ups are integrated (D297, D315–D319, D369), with retained BIND and Knot evidence (D398) and elected-startup sequencing (D411). See the remaining coverage and startup items below | D170; F28; #135 | v0.2 |
| A local copy of the root zone (RFC 8806) for recursive groups | D170 | later |

The [November 20 refusal and fencing regressions](milestones/v0.2/followup-audit.md#f28-open6-mounted-refusals-and-fenced-metrics--november-20-2026)
cover all six refusal reasons and the fenced gauge transition (#169). The
startup timing and historical ledger review remain separate in #135.

## Auth

| Item | Source | Target |
|---|---|---|
| Login lockouts: shared counters on Kubernetes (D304, D310) and complete SQLite (D305–D309); a follower forwards local login (D363), sign-out and own sessions (D368). Open: OIDC, browser and device logins on followers, and F46's final gates | dev-docs/milestones/m3/auth.md; F46 | v0.2 |

## Security (dev-docs/milestones/m5/security-v0.1-round.md)

| Item | Source | Target |
|---|---|---|
| Repository settings for the release pipeline (cannot live in the repository): a branch ruleset refusing `v*` branch names, required review with CODEOWNERS on `release` and `release-candidate` (`.github/CODEOWNERS` for `/.github/` and `hack/release-tag.sh`), deployment branches for a `release` environment | dev-docs/milestones/m5/security-v0.1-round.md | release operations (verify for each release) |
| Release promotion rules: owner applies the [reviewed settings proposal](release-checklist.md#release-pipeline) and records a passing read-only check of both branches, including any classic-protection access gap. The proposal and dry run do not verify applied settings. | [#140](https://github.com/robertherbaugh/kudurru/issues/140), part of [#133](https://github.com/robertherbaugh/kudurru/issues/133) | before v0.2.0 promotion or publication |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class: the class-parameterized workflow and collector are implemented and the owner chose Ubicloud 8-core (F63). Open: the paired and fixed-class runs on the final head (G23, D383); earlier paired runs failed cache hit and Set latency | blocking, store-kube spikes; hardening report; F63 | v0.2 |
| G23: D416 proposes an opt-in, original-reference-only host preflight before D412 timing. Owner adoption is pending; workflows and qualification remain unchanged by default | [Host-equivalence preflight (proposed)](../bench/README.md#host-equivalence-preflight-proposed); #176 | v0.2 |

## Deployment (binary behavior the chart and compose files need)

| Item | Source | Target |
|---|---|---|
| `kudurru_dnssec_key_expiry_timestamp_seconds` for DNSSEC signing keys (F64c, the original v1.0 subtarget). `snapshotAge`/`leader` in `/healthz` (F64b) and the SQLite fencing metric, panel and alert (F64a) shipped | deploy report; F64c | v1.0 (DNSSEC signing) |

## Needs the project owner's hardware

The owner deferred the independent two-VM keepalived test to follow-up on
November 9, 2026 (D358): no disposable VM pair exists. Keep the original test
below unqualified; container and kind/MetalLB checks retain their own evidence.

| Item | Source | Target |
|---|---|---|
| Spikes (a) dnsdist console directives under YAML, (b) PROXYv2 + chashed with a real dnsdist and `kdig +quic/+tls/+https`, (h) keepalived 2.4.3 on two VMs (lameduck keeps the VIP, `vip drain` moves it, node loss moves it). Spike (f), kind + MetalLB, runs on GitHub's runners in the kind workflow (hack/kind-e2e.sh) | week-1 spikes, dnsserver spike | deployment acceptance (dev-docs/release-checklist.md §3) |

## End to end and fuzzing (dev-docs/milestones/m5/e2e.md)

Bugs found by the M5 e2e package. E2E-1, E2E-3, E2E-4, E2E-5 and E2E-6 were fixed after M5 (default-group hints fall through to conditional forwarding; `node.lameduck` drain window, default 2s; `keys.Load` reports `Generated` only for a key it created; readiness waits for the first live snapshot; keepalived paths allow braces only as `{vrid}`, 0a25647) and their tests now assert for real. The remaining items keep a fuzz-input filter or a design note naming the id.

| Item | Source | Target |
|---|---|---|
| e2e still missing from §13.1: the two-VM keepalived failover, deferred by the owner (D358; see "Needs the project owner's hardware" above). The Knot catalog consumer (F68b, `6aa58ed`) and the AdGuard Home black-box corpus (F68a) are integrated. The pinned Knot and dnsdist suites also passed on hosted private Docker daemons in [run 44811568053](https://github.com/robertherbaugh/kudurru/actions/runs/44811568053) (D383) | dev-docs/milestones/m5/e2e.md, dev-docs/milestones/m5/features-v0.1.md | verify 2-VM failover for each deployment (dev-docs/release-checklist.md §3) |

## License portal (license-server/)

| Item | Source | Target |
|---|---|---|
| Step-up re-authentication (a sign-in within minutes, `max_age`) for granting roles and other superadmin actions | design critique, security | after go-live |
| Signing-key lifecycle in the portal: several keys, "trusted since" from release tags, a compromised flag and bulk re-issue under a new kid | design critique, completeness | when a second kid exists |
| A signer outside the portal's process (Vault transit or a separate container) behind the `Signer` interface | design critique, security | when the portal grows beyond one operator |
| Expiry reminders by mail (45, 30 and 7 days) and mail to the requester when a request is decided in bulk | product research | after go-live |
| RP-initiated logout, several identity providers, a metrics endpoint, audit export | design spec | later |
| Dashboard `LicensePage` dates in UTC, and the "no restart" wording in LicensePage, the website, licensing.md and helm.md: the flag and `KUDURRU_LICENSE_KEY` are read once at startup | design critique, product | next licensing round |
| `license.Manager` logs "license key replaced" when a key with the same jti but other claims or expiry arrives (today it logs only state, id or source changes) | design critique, compatibility | next licensing round |

## v0.2 completion pass: open at 68a709e2

Open findings of the packages the owner had merged on November 11, 2026
before their verification was complete. Each section of the
[completion plan](milestones/v0.2/completion-plan.md) ends with the package's
state; this table lists what is still to do. Close a row by deleting it and
citing the commit.

| Item | Source | Target |
|---|---|---|
| SQLite upgrade: the `503` an API write can get just after readiness is handled only by the test; `TestRunServeSQLiteUpgradeWithLegacyPlaceholders` failed in the composed run; a merged CNAME's TTL now changes on upgrade (stated in the changelog's upgrade notes, but no test covers it for CNAME) | QUAL-SQLITE-UPGRADE-2 verify | v0.2 |
| Review nits with no recorded state (the fixer neither addressed nor rebutted them): DOCS-ARCH, three (the `cliClientID` CRD field comment still says "`kudurru login` (v0.2)"; the corrected group tooltip and host-name help have no asserting test; `cmd/kudurru/doctor_notify_kube.go` still calls the credential source an immutable startup inventory); QUAL-KUBE-UPGRADE, two (`TestEnvtestCRDUpgradeFromV01` creates no legacy IAM Secret, so that path is covered at process level only; its ledger named a base commit that its first three commits precede); INT-DEL-C, one (`docs/deploy/integrations.md` "When the license expires or is suspended" mixes `LicenseUnavailable` with the Integration's own `Suspended`) | completion-pass journals | v0.2 |
| Fix commits never verified: DOCS-ARCH | completion-pass journals | v0.2 |
| Observed by the docs lane on November 10 and not rechecked at 68a709e2: on SQLite a signed update is answered `SERVFAIL` instead of `REFUSED` when the zone or the key's grants do not allow it; `nsupdate` reports a wrong TSIG key as "clocks are unsynchronized" (zero signing time in the error answer); `cluster status`, `nodes` and the license node count describe only the answering node; an enrolled cluster issues at most 64 invitations in its lifetime; a Kubernetes backup and restore recreates `rfc2136-<zone>` as API-owned (from the code) | DOCS-OPS | v0.2 |
| No audited operator revocation on the primary for a root-key revocation it can never record | F28-HISTORY-RECHECK | owner decision |
| An unpinned Kubernetes node exits during an API-server outage instead of staying up and unready (G22-WP4 item 2); the fencing loop in `kube_cluster_test.go` counts a failed `/healthz` as not leader | QUAL-KUBE-UPGRADE (D373) | v0.2 |
| Data race between `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones`'s `opts.Now` replacement and the integration host's refresh | HA-FIX | v0.2 |
| Paired benchmark and fixed-class run on the final head; PR body | G23, G25 | v0.2 |

G23 diagnostic (#175, `release_qualified: false`): the
[retained-vector diagnosis](milestones/v0.2/final-acceptance.md#retained-vector-diagnosis-175)
confirms same-source drift beyond the CNAME limit; local profiling isolates
no source cause. Hit p99 merits a source-isolating diagnostic before any
separate performance fix. The failed recording stands and G23 remains open under
[#143](https://github.com/robertherbaugh/kudurru/issues/143).

## v0.2 completion pass: open at 17b7c87a

Open findings of the packages merged after 68a709e2, up to the qual lane head
17b7c87a (the PR branch at 85913663 plus RPZ-OPEN). The
[completion plan](milestones/v0.2/completion-plan.md) has each package's state.
Rows of the section above that this pass closed were deleted: the hosted
failures of 68a709e2 (CI-RED, `80747d40`, `2074cc8c`, `c12125ac`), the free-port
race and the adapter/auth-cookie failures (FLAKE-PORT-RACE, FLAKE-PRIMARY-UNAVAILABLE),
the long-apply gap (CORE-APPLY-OPEN, CORE-OPEN3; residual below), the over-broad
D377 (CORE-RETAINED-SERVFAIL), the status-write SERVFAIL (CORE-STATUS-WRITE-SERVFAIL),
the missing live follower-behind-edges test (F28-EF1-LIVE, `de627369`), the
cleanup partial-receipt regression and INT-DEL-B's follow-ups (INT-OPEN, `055353fb`),
the dnstap and OBS-FANOUT gaps (OBS-OPEN) and the overtaken docs (DOCS-RECONCILE);
the never-verified fixes of HA-REFUSAL and OBS-HTTP-SINKS were verified by HA-OPEN
and OBS-OPEN, and a refusing member whose primary is lost can abandon the lost
term (F28-HISTORY-RECHECK, D369).

For the readiness, long-apply, and status-write rows below, see the
[current local #172 evidence](milestones/v0.2/evidence/2026-11-20-issue-138/172-loaded.md).
It exercises mounted multi-P readiness credit, reproduces second-reader
`generation_pending` without race, and records fresh status-write probe
counts. The table retains the historical observations and missing logs;
neither the original 1.39 s DNS gap nor the owner's lab rate is resolved.

For #131/#164, the informational `sinks` job runs both mandatory live
tests against pinned ClickHouse 25.8 and Loki 3.7.9 on code changes. Its
logs identify the tested source and exact server versions. The first
[hosted `sinks` job](https://github.com/robertherbaugh/kudurru/actions/runs/44802230354/job/136517302300)
passed both tests with `KUDURRU_REQUIRE_SINK_SERVERS=1` on November 20,
2026: PR #185 head `6b4b958b`, tested merge source `418f8ede`, ClickHouse
`25.8.33.6`, and Loki `3.7.9` (`d3b4a2f7`). The earlier
[final-source result](milestones/v0.2/final-acceptance.md) is evidence for
`a22255a2`, not a hosted result for this job. Adding `sinks` to `ci-ok`
requires owner approval.

Current K3/K7 follow-through: [mounted Kubernetes evidence for #156](milestones/v0.2/followup-audit.md#k3k7-mounted-evidence-156)
supersedes the historical test gaps below for election, shared budgets,
holder recovery, partition and re-acquisition, cleanup fencing, rotation,
license expiry and UID replacement. This is local envtest evidence only;
it does not change deployment support or claim live qualification.

| Item | Source | Target |
|---|---|---|
| Define what `kudurru integrations list/get -o name` should print. The current list prints a table and get prints YAML; #154 preserves both formats. | PR #192 review | Integration CLI follow-up |
| Readiness under CPU starvation: the readiness budget of `startStaticMount` has no run that turns a failing condition into a pass (under the reproducing condition the test fails at its own 5 s HTTP timeout), the multi-P credit is not exercised, and the other mounted tests keep fixed 20 s readiness loops | CI-RED verify; FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| Long multi-item request on SQLite: an unexplained 1.39 s DNS gap in about one run in a hundred without race (and one refused read in 1 of 15 runs on one CPU); under race on a loaded machine most of a second caller's reads are refused with `generation_pending`; the non-race run is a CI gate with thin margin; `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` fails about half the time without race on the loaded lab machine on its read assertion | CORE-APPLY-OPEN and CORE-OPEN3 verify; integrator | v0.2 |
| Status-write SERVFAIL fix: the fixer's historical mounted and lint evidence is incomplete (`fix-03-mounted.log`, `fix-04-lint.log`). The comment now reports 0 to 10 silent probes across 24 changes and calls the bound coarse (`77d2c9cb`, `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`); loaded evidence is tracked in #172. | CORE-STATUS-WRITE-SERVFAIL verify | v0.2 |
| Tier-B hooks and webhook records: the mounted 24 h expiry and budget-exhaustion cases, a mixed-version test for the IntegrationEventCursor kind and a negative run of the A1 audit rows are missing; with no Integration lease running nothing prunes; a hookless owner whose lease restarts more often than every 15 minutes never runs retention; deferred topics `query.blocked`, `vip.master_changed`, `client.seen`, `client.left`; no per-namespace event filter | F60C-HOOKS, F60C-OPEN | v0.2 (topics later) |
| Kubernetes integration host: K3 and K7 stay partial (two-replica budget sharing with hand-placed Leases only, credential rotation proven by order only, no holder-change, partition or mid-cleanup lease-loss qualification, expired license and re-created Integration proven at the adapter only); UniFi record publication and Tier-B webhooks are refused on Kubernetes; native Secret credential references are disabled; the CLI `get` is not run on Kubernetes | F09B-KUBE-ROOT, F09B-KUBE-SOURCES, F09B-OPEN | v0.2 |
| Edge blocks: the five-minute restore hold measures wall time since the first lower sighting, so two pulls five minutes apart accept a stale revision at once; after a deliberate restore new blocks are delayed up to five minutes (not in the runbook); an owner whose only backend misses a pull has its blocks cleared and reinstalled | EDGE-DYNBLOCK verify | v0.2 |
| Known issue: the pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3 request whose QUIC FIN arrives in its own empty frame. The live client-ID test's canary fails once an image bump fixes it (then drop the retry and the known-issue text); the canary cannot tell the defect from another failure; one DoQ loss was attributed to the defect by inference only | EDGE-CLIENT-ID | dnsdist upstream; after v0.2.0 |
| CoreDNS same-name multi-port loss reporting (#161) and Kubernetes RPZ policy-state counts and readiness (#162) are fixed. The [RPZ-OPEN follow-up](milestones/v0.2/followup-audit.md#sqlite-restart-and-pending-filters-163) measured local SQLite restart windows of 152 ms and 303 ms from first response to first RPZ NXDOMAIN (no NOERROR), limited pending filters to RPZ, and pinned the CLI fixture messages. The signed Kubernetes offline restart still fails closed by design (D390). | RPZ-KUBE, RPZ-DIAG, RPZ-OPEN; #130, #161, #162, #163 | Implemented; see the [verified closeout](milestones/v0.2/final-acceptance.md#verified-issue-closeout-november-21-2026) |
| Settings after the first 0.2 start: `PreparedImage.CommittedDynamicOwnership` rebuilds the zone's dynamic index on every publication; `docs/deploy/runbooks.md` (near line 600) and `docs/deploy/compose-ha.md` (near line 108) say a later file edit of any cluster key is not applied, which is inaccurate for restart-required keys; static twins cannot change a cluster setting after their first 0.2 start; `TestAnchorUpgradeStaticTwinV010/LaterFileEditNotApplied` does not assert the seeded value | QUAL-OPEN2 and QUAL-MIXED-VERSION verify | v0.2 |
| G21-PERMISSIONS (#139), closed with local evidence at `62763b3750cbaf025407fbec7d11a6c778ae1fdf`: five same-package negative controls fail on `c36efe14` and `ce3ab6e2` and pass with the existing six after the fix. The 75 verbose `uses, first` inventory lines match both baselines. `CGO_ENABLED=1 go test ./internal/api -run '^Test(RoutePermissionLint\|HandlerPermissionScanCreditsOnlyChecks\|PermissionVerbsAreUsedOrReserved\|LintVerbUseCatchesUnusedAndStale)$' -race -count=1 -v` passed (4 tests, 11 subtests); `CGO_ENABLED=0 go vet ./internal/api` passed; `CGO_ENABLED=0 golangci-lint run ./internal/api/...` (v2.15.0) reported 0 issues; `python3 -B -m unittest discover -s hack -p 'test_bench_*.py'` passed all 53 bench-guard tests on an isolated rerun after one `Text file busy` fixture failure. These results replace the missing evidence without reclassifying the empty historical `fix-bench01.log` and `fix-lint02.log`. The bare-name item is closed; D393 in `dev-docs/design-record.md` already closes the `settings.operatorSeesQueryLog` wording item. The review follow-up adds range-declaration and method-receiver controls: both fail on `7c3f1355`, all 13 scanner subtests pass under race after the follow-up, and the 75 inventory lines still match. No hosted acceptance or performance claim. | G21-PERMISSIONS verify | v0.2 |
| Evidence gaps: FLAKE-PORT-RACE's ledger points at unfinished logs (use `verify-01-mounted-race.log`, `verify-03-lint.log`) | FLAKE-PORT-RACE verify | v0.2 |
| Low-severity notes left open: OBS-TRACING (about 5 KB per traced query); F09B-OPEN (joined budget error classification untested; timing-dependent bounds); QUAL-OPEN3 wording of the measured percentages | verify results in `followups/*.json` | v0.2 |

RPZ diagnostics (#162): mounted `TestRunServeKubeRPZFeeds` already reports
`blocked_rpz`, `list`, and `rule` through HTTP dig. The same test reproduced
missing policy-state rule counts and incorrectly ready unavailable feeds;
it now pins the node-local projection alongside the equivalent assertions in
`TestRunServeSQLiteRPZFeedDiagnostics`. Evidence is local (envtest and SQLite).

The historical HA-SELF notes were addressed in the HA-OPEN5 batch (`7bae1238`,
`532a5cd7`) and its review fix (`1737eb2e`).
`TestRootMemberForwardPasswordTransportRefusalActualHTTP` distinguishes a
retained password refusal from a member transport refusal and checks that
neither leaves a durable mutation audit row. See the
[current #137 evidence](milestones/v0.2/followup-audit.md#follower-self-service-refusal-evidence-137).

### Current F09B evidence (#174)

The historical F09B entries above describe budget classification as untested
and credential rotation as proven by order only. These two evidence gaps are
closed locally: the [host and envtest runs](milestones/v0.2/evidence/2026-11-20-issue-138/174-integration.md)
pin the error classes and distinguish rotation cancellation from the request
deadline. K3/K7 live qualification remains open, as does this diagnostic:

| Item | Source | Target |
|---|---|---|
| Sync reports local budget contention (`ErrConflict` and `DeadlineExceeded`, no outbound request) as `ReconcileFailed` / `ErrProvider`. The tests pin the existing class; no behavior change is included. | F09B-OPEN; #174 | v0.2 |

## v0.2 completion pass: open at 7663c957

Open findings of the packages merged after 17b7c87a, up to the PR branch as
pushed at 7663c957 (November 14, 2026). The
[completion plan](milestones/v0.2/completion-plan.md) has each package's state
in its third-splice section. Rows of the section above that this pass closed
were deleted: "rejoin acquisitions 5" (F28-REJOIN-COUNT, `3beab976`,
`1a600459`, `70172b92`) and the fixture data race in
`TestAnchorEdgeActivationJoinedFollower` (`4a58f33a`); UDP answers dropped
across a publication swap (D395, `f0d2bca0`, `444ec28c`); the dashboard
Integrations page (DASH-INTEGRATIONS, `0244149c`); the zone recreated by a
group member's PUT and the 200 with zones left out (CORE-OPEN4, `ce1ef55c`,
`40ca9884`) and the impersonation match (`77c04800`); the audited `GET /cluster`
poll (DASH-OPEN, `7cc633f6`) and the missing follower tests of six dashboard
surfaces (`86f437ab`). The other deleted rows are restated below with what
remains.

| Item | Source | Target |
|---|---|---|
| Hosted CI: the `ci` workflow passed once, on `bae1c9df` (run 43532858454); the `docs` and `security` workflows failed on that head (fixed by `cb04067b` and `7663c957`, SAST annotation not rerun locally); no result is recorded for `81c2d3af` or `7663c957`. Every other pushed head after 17b7c87a failed (`d063f929`, `daff96e5`, `a3b5a1d0`, `c5b0637c`, `95c57113`, `2e15060e`, `531bffd0`); the tests are in the completion plan | integrator's log | v0.2 |
| The 503 right after a node reports ready: a sign-in, read or write on a ready node can be refused before dispatch with `503 generation_pending` or "current writable serving generation unavailable" while a publication is prepared (publication restarts its preparation on every generation or Wake). D399 (command recapture) was merged and reverted (`827a6ebe`, `07f56d53`) because seven command tests that require a stale command to be refused failed with it; D405's first five commits were reverted (`bae1c9df`) because they broke every enrolled cluster process test. Redo both so that the full `./cmd/kudurru` suite and the enrolled e2e tests pass; decide whether a POST answered with that 503 is truly effect-free (CORE-OPEN4's test accepts a later 409 already-exists) or must be `outcome unknown`; remove the test-side retries in `serve_zone_change_mount_test.go` and `serve_commit_gap_test.go` and the e2e quiet period `enrolledQuietReview` | D399-REDO, PUBLISH-STARVATION (D405), CORE-OPEN4, F28-OPEN7 verify | v0.2 |
| Through an enrolled follower, one signed UPDATE sent once was answered SERVFAIL after 330 ms (fixer's `fix-e2e02.log`, `TestEnrolledClusterProcessesPartitionCatchupAndRestart`), cause unknown; through a newly promoted primary the bound SERVFAIL is tolerated by up to four test resends per update; no `./cmd/kudurru` test exercises the production four-second bound under race | HA-OPEN6 and HA-OPEN5 verify | v0.2 |
| `TestRunServeUniFiHostnamePolicyLifecycle` fails under the race detector since the ID-INVENTORY commits (bisect: passes at `2e15060e`, fails at `e0bddb72`) | ID-INVENTORY verify; integrator; INVENTORY-REGRESS | v0.2 |
| Managed root anchors: the [November 20 evidence](milestones/v0.2/followup-audit.md#f28-startup-bounds-and-hosted-bootstrap-evidence--november-20-2026) confirms the exact bootstrap count and two-race cap on current source. Hosted CI 44780132339 observed one acquisition and zero races in each enrolled case. The original 10 s readiness polls and the restarted-writer test passed five local race repetitions; the historical heavier-load misses and early writer return were not reproduced. Pre-publication SERVFAIL remains possible (538 ms in that hosted run, under the unchanged 5 s sampler limit). Full hosted command coverage remains open. Mounted refusal/fence coverage is tracked in #169 (PR #205); retired promotion and abandoned-term return are tracked in #170 (PR #215) | F28-OPEN6, F28-OPEN7, F28-REVOKE-HOSTED verify; #135 | v0.2 |
| IAM writers: D413 replaces the owner-rejected permanent-history limit with the approved checkpoint. D414 records owner acceptance of v0.2 token issuance with both permissions and the mounted TLS, final-clock, provider and follower proofs. Additional local process acceptance covers real member-mTLS replication and follower restart with the primary stopped; unrelated administrator sessions survive. Live Authentik password/TOTP authorization-code login additionally passes on both processes with actual replication; manual browser UI and independent-host behavior are not claimed. See [the scoped acceptance report](milestones/v0.2/local-release-acceptance.md). Shared live-state bounds remain | IAM-WRITERS; D413/D414 | v0.2; scoped proofs recorded, independent review separate |
| Integrations: SQLite capture and changing commands still read the whole generation at the original 10 s / 30 s bounds. The [partial local matrix](milestones/v0.2/evidence/2026-11-20-integration-scale/README.md) observed a loaded capture deadline and cleanup cutoffs; it does not establish a production threshold (#151, #124). Bounded cleanup progress and restart evidence are tracked separately (#152). Permanent Provision/Validate/Check errors and the fenced setup branch are retried every five minutes indefinitely; the historical 30 s stall in `TestIntegrationUniFiPushMirrorPartialCreate` is unexplained but did not reproduce in five current-main race runs (see the INT-UNIFI-PUSH follow-up for the separate observation 503); hosted-intermittent `TestIntegrationWebhookMountedClientsRestart` and `TestIntegrationDeletePurgeFencesAChangedObjectAndFinalizeRefusesLeftovers` | INT-OPEN2 verify; integrator | v0.2 |
| UniFi optional writes: the mounted TLS double covers partial update/delete settlement, lost acknowledgments, restart and lease loss, and the deleted-source 409. The host abandon regression covers acknowledged creates. Live UniFi and Cloudflare acceptance remains pending owner authorization, disposable resources, and sanitized cleanup receipts | INT-UNIFI-PUSH verify; #125 and #153 | v0.2 |
| External zones: #123 adds source conditions and metrics; #150 adds smaller-page retry and Service selector compatibility, without changing the caps. See the local RBAC recovery, pagination and selector regressions in the follow-up audit. | INT-EXTERNAL-ZONES verify | v0.2 |
| Client-group views (#136): local SQLite runtime and Kubernetes envtest qualify UPDATE ownership in both namespaces; mounted SQLite AXFR and IXFR use default data with either allowed TSIG key. The SQLite takeover is documented and covered by `TestSQLiteUpdateViewOwnership`. The dashboard namespace edit and remap tests and explicit `source: served` API contract supersede the older editor and heuristic findings. Partial SQLite refusal visibility is covered by the merged #171 fix; pinned live edge TCP still needs execution (#127), and a namespaced forwarded delete is refused by an older strict primary | INT-6 and DASH-INVENTORY | v0.2 |
| Parent and child zones: a new child zone answers SERVFAIL until its first publication (up to 5.2 s without race, 27 s under race), not within D395's bound; `TestRunServeSQLiteParentEditedAsChildIsCreated` used half its bound on a loaded machine | CORE-CHILD-RECONCILE, CORE-OPEN4 verify | v0.2 |
| Zone groups: a Kubernetes delete between `writeObject`'s read and `Apply`, and the SQLite list 503, are untested | CORE-OPEN4 verify | v0.2 |
| Commit gap: the mounted test covers no parent/child pair, forward upstream-group change or tightened ACL (unit tests only) | CORE-COMMIT-GAP verify | v0.2 |
| Refusal inventory: the process-level fenced and follower walks are each proven by one pass. Local starved standalone and static walks exhausted the unchanged five-minute harness context and separately saw undeclared `503 generation_pending` on auth operations. `followerUnsentMessage` is tolerated as transient with its removal condition only in a comment; no green full `./cmd/kudurru` run exists (acceptance item 4); the hintless check misses non-literal statuses and hints. | G21-REFUSAL-MERGE, G21-REFUSAL-INVENTORY verify; [#172 evidence](milestones/v0.2/evidence/2026-11-20-issue-138/172-loaded.md) | v0.2 |
| Release checks: the full goreleaser snapshot has not run (needs about 14 GB free; about 11 GB were free) and the image check ran only against a fake docker; `hack/live-recursive-node.sh` still exits without the node log when dig gets no answer; `hack/snapshot-images.sh` can remove a version-tagged image that existed before a failed run; shellcheck not run | G24-LOCAL verify | v0.2 (G24) |
| Dashboard low notes: the Lists page rereads every 3 s while any feed is not serving; a session whose `/auth/me` still claims a withdrawn `cluster.read` sends one audited `GET /cluster`; no browser test against a real enrolled follower; `PromoteDialog` and `ActivateDialog` share the report-by-effect pattern; the identity guide overstates disabled rows; the role-binding route's bare 503 cannot tell a refusal from an unknown commit; saving a stored Cloudflare zone drops explicit `false` values | DASH-RPZ, DASH-OPEN, DASH-CLUSTER, DASH-IAM, DASH-OPS, DASH-INTEGRATIONS verify | v0.2 |
| Dangling D399 and D405 references: no decision row exists for either at 7663c957, yet `docs/deploy/sqlite-join.md` (line 193, "a command superseded before its dispatch (D399)", restored with HA-OPEN6 by `832dab99`) and the comments of `cmd/kudurru/serve_zone_change_mount_test.go` (line 106, D399) and `cmd/kudurru/serving_member_update_stale_test.go` (lines 326 and 334, D405) cite them as if they existed. Reword them, or keep them and add the rows when D399-REDO and PUBLISH-STARVATION land | LEDGER-SPLICE-3 review; integrator | v0.2 |
| Import step dispatch: the re-capture before a step's body (`57cef0e6`) refines the dispatch-once rule without a decision row (D399 was to generalize it and is not at this head); a later import step is no longer bounded by 5 s | FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| Intermittent tests seen at least once in this pass, not skipped: see the third-splice section of the completion plan (the 503 family, managed-anchor readiness, integration tests, `TestCLIOIDCBrowserActualTLSProofRefusals`, `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce`, `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`, `TestRunServeKubeRPZFeeds`, `TestRunServeSQLiteTOTPEnrollmentRestartRecovery`, `TestRunServeSQLiteCompoundZoneCommands` and others) | integrator; verify results | v0.2 |

### Runtime refusal coverage (#173)

- `0e07ff47` corrected `UpstreamContacted` to mean that an authority was asked;
  `TestFailureUpstreamContacted` covers a non-deadline cache admission refusal
  after contact, and `54089469` pins its retained-output class in
  `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`.
- `8610d4f3` made rejected replay admission a final decision. Both
  `TestMemberUpdateRerunOnlyWhenNothingNewWasDecided` and
  `TestRootMemberUpdateReplayRefusedByAdmissionIsAnsweredAtOnce` pin it.
- The terminal retiring-zone capture is covered on the forwarded path by
  `TestRootMemberUpdateForRetiringZoneIsAnsweredAtOnce` (`152c7369`) and on the
  native path by `TestPrimaryRuntimeNativeUpdateForRetiringZoneIsAnsweredAtOnce`.
  Removing the terminal branch makes the native test report a stale recapture.
- `RunnableStatus.LastError` retains an error recorded in a live term, but an
  error that returns after cancellation does not create or replace one, as
  intended by `0da1aac2`. The two `TestRunnableLastError` tests pin both cases.

See [local regression and negative-control evidence](milestones/v0.2/evidence/2026-11-20-issue-138/173-runtime.md).

### Owner disposition for #129

On November 20, 2026, the owner accepted D395 and D375 as implemented and
selected documented member replacement for v0.2. The retirement command
(#160) stays deferred. See the [release disposition](milestones/v0.2/final-acceptance.md#owner-disposition-for-129).
This does not change the independent root-key revocation follow-up or the
D358 deferral.

### Local follower apply evidence (#159)

On November 20, 2026, at `61916ce557be788f5731955405dad014743fd9e3` with the
#159 test changes, the following local race runs passed (logs in the workspace):

| Command | Result | Log |
|---|---|---|
| `CGO_ENABLED=1 go test ./cmd/kudurru -run 'TestRootMember.*Apply' -race -count=3` | Pass, 78.906 s | `.context/gh159-apply-race.log` |
| `CGO_ENABLED=1 go test ./cmd/kudurru -run 'TestRootMember.*Apply\|TestMemberItemRefusal' -race -count=3 -v` | Pass, 79.359 s | `.context/gh159-apply-refusal-race.log` |
| `CGO_ENABLED=1 go test ./internal/cluster/replica -run Hold -race -count=1 -v` | Pass, 2.762 s | `.context/gh159-replica-hold-race.log` |

The lifetime case reports `created`, `completion_unknown` for a committed
document whose receipt was not relayed, and two `unavailable` items that
are absent on the primary. The held-pull case reports `created` followed
by three `unavailable` items that are absent on the primary. Both cases
check the original primary and count member commands before deduplication;
none is replayed. The held-pull refusals and the uncertain result name the
primary; the existing lifetime-expiry refusals do not. No wording, production
timing, or 200-document process-test expectation changed. These runs are
local evidence only; no live or independent-host acceptance was run.

On `ce3ab6e2` with the primary-name assertion added, the lifetime case failed
for both unsent items. With the wording fix, those items also name the
original primary. The cutoff gates, durable SQLite checks, original command
deadline, and pre-deduplication counts are unchanged. Local checks:

| Command | Result | Log |
|---|---|---|
| `CGO_ENABLED=1 go test -p 4 ./cmd/kudurru -run 'TestRootMember.*Apply\|TestMemberItemRefusal' -race -count=3` | Pass, 112.574 s | `.context/gh159-primary-race.log` |
| `CGO_ENABLED=1 go test -p 3 ./internal/cluster/replica -run Hold -race -count=1` | Pass, 3.049 s | `.context/gh159-primary-checks.log` |

### Tracing/cache failure attribution (#165)

The [hosted failure](https://github.com/robertherbaugh/kudurru/actions/runs/43514296652/job/132192391544)
on November 13, 2026, tested `531bffd0`. The `go test 1/2` log reported:

```text
--- FAIL: TestRunServeTracingThroughCacheAndRace (4.52s)
    serve_tracing_cache_test.go:218: trace 7479c20d94664b9ce2cff08616a423c2: span kudurru.dnssec ends before it starts
```

The log has no raw OTLP timestamps to attribute that individual failure to
a particular interleaving or clock change. A second failure mentioned in
the audit has no attributable run or assertion in the retained evidence.

On November 20, 2026, the unmodified race-enabled test passed 30/30 on
`ce3ab6e2` with five available CPUs and 30/30 on CPU 0 with `GOMAXPROCS=1`.
A temporary `go test -overlay` on the same source forced the detached
prefetch to start its `kudurru.dnssec` span after the answer cutoff was
captured but before `Trace.Finish`. The still-open span got an end before
its start: wall delta −90,058 ns, monotonic delta −90.056 µs. The unchanged
mounted test failed at the same assertion. The same forced ordering
passed after clipping detached span timestamps to the answer. This is
local, controlled reproduction of a mechanism, not a reconstruction of
the original hosted timestamps. No deadline or observation was relaxed.

The deterministic unit regression and the tracing and pipeline suites
passed under race with the fix. The local `BenchmarkTracedChain` allocation
remeasurement on `ce3ab6e2` and the fix is in the
[tracing guide](../docs/deploy/tracing.md#what-tracing-costs-a-query).
The four existing slow-exporter regressions also passed locally under race:
`TestSlowExporterNeverBlocksFinish`,
`TestSinkFanoutBlockedSinkNeverStallsPrimaryOrTail`,
`TestHTTPSinkStalledDestinationLosesOnlyItsOwnQueries`, and
`TestRunServeQuerylogSinksStalledFailingAndReload`.

### November 20 managed-anchor term-rejoin regressions (#170)

The [mounted term-rejoin evidence](milestones/v0.2/followup-audit.md#f28-open7-retired-promotion-and-abandoned-term-return--november-20-2026)
covers promotion with a pending local denial and the lost primary's return
after explicit offline term abandonment. Startup/load and hosted evidence
remain tracked separately in #135.
