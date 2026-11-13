# Follow-ups

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
package verdicts in the [acceptance table](milestones/v0.2/gaps.md#feature-package-acceptance-at-154a779a-november-13-2026)
still apply; deleting a row closes the follow-up, not the package. Rows whose
work is not complete stay below with the remaining part restated; none was
retargeted except F64c, whose remaining metric was already the original v1.0
subtarget.

## Zone, controller, objects

| Item | Source | Target |
|---|---|---|
| Stored TSIG keys: SQLite and Kubernetes management with hot reload (D292, D314, D365) and the `kudurru tsigkeys` CLI (D394) are integrated, and TSIG on DoH/DoQ/DoH3 is integrated (F07b, `a6bf8e1`). Open: `kudurru tsigkeys` has not run against a Kubernetes node, and F07a's final gates (G21–G24) | dev-docs/milestones/m2/xfr.md; F07a | v0.2 |
| DS at a delegation point where parent and child are both local routes to the child; needs a DS special case | zone report | v1.0 (DNSSEC) |

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
| Query-log summary and client counts can omit newly written previous-hour events when a one-hour window ends in the first minute of an hour; add deterministic boundary coverage and correct rollup initialization or reads | PR #75 local validation; reproduced on `origin/main` with `TestClusterStatsMerge` and a window ending at HH:00:30 | Next query-log correctness pass |
| Lazy-DFA regex engine integrated (F20d, D293). Open: regex-1000 p99 stays adverse and the original relative benchmark gate exits 1; corpus and scale qualification stays in G23 | blocking spike; F20d | v0.2 |
| Transfer chunks are budgeted by uncompressed size (69 messages of ~42 KB for 100k records); budget by compressed size for fewer, fuller messages | dev-docs/milestones/m2/xfr.md | later |
| Multi-socket DoT (F25a, `eeb68c2`) and CID-routed DoQ/DoH3 (F25b, D311) are integrated. Open: one local sample worsens HTTP3 by 12.54%; fixed-class performance and final release checks (G23) | hardening report; F25b | v0.2 |
| Settings hot reload: pseudonym identity (F26a), cache shard growth (F26b, `62990dd`), blocking refresh and degraded settings (F26c, `38153fa`), query-log sinks (F26d) and the five auth controls on complete SQLite (F26e, D290) reload without a restart. Open: on Kubernetes and for host inputs the auth controls stay restart-bound (`restartRequired` names them); privacy mode and method changes keep separate lifecycles | dev-docs/milestones/m5/burndown-server.md; F26e | v0.2 |

## Recursive resolver (D170)

| Item | Source | Target |
|---|---|---|
| Automatic root trust-anchor updates (RFC 5011): managed anchors are integrated (D297, D315–D319, D369) and qualified against BIND and Knot authorities (D398). Open: F28-OPEN6 was backed out and F28-OPEN7 is in progress (writer-admission regression, exact acquisition counts, the 10 s SERVFAIL tolerance); see the open rows below | D170; F28 | v0.2 |
| A local copy of the root zone (RFC 8806) for recursive groups | D170 | later |

## Auth

| Item | Source | Target |
|---|---|---|
| Login lockouts: shared counters on Kubernetes (D304, D310) and complete SQLite (D305–D309); a follower forwards local login (D363), sign-out and own sessions (D368). Open: OIDC, browser and device logins on followers, and F46's final gates | dev-docs/milestones/m3/auth.md; F46 | v0.2 |

## Security (dev-docs/milestones/m5/security-v0.1-round.md)

| Item | Source | Target |
|---|---|---|
| Repository settings for the release pipeline (cannot live in the repository): a branch ruleset refusing `v*` branch names, required review with CODEOWNERS on `release` and `release-candidate` (`.github/CODEOWNERS` for `/.github/` and `hack/release-tag.sh`), deployment branches for a `release` environment | dev-docs/milestones/m5/security-v0.1-round.md | release operations (verify for each release) |

## Build, CI, docs

| Item | Source | Target |
|---|---|---|
| Bench job on a fixed runner class: the class-parameterized workflow and collector are implemented and the owner chose Ubicloud 8-core (F63). Open: the paired and fixed-class runs on the final head (G23, D383); earlier paired runs failed cache hit and Set latency | blocking, store-kube spikes; hardening report; F63 | v0.2 |

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
| e2e still missing from §13.1: the two-VM keepalived failover, deferred by the owner (D358; see "Needs the project owner's hardware" above). The Knot catalog consumer (F68b, `6aa58ed`) and the AdGuard Home black-box corpus (F68a) are integrated; Knot runs only on a local private Docker daemon (D383) | dev-docs/milestones/m5/e2e.md, dev-docs/milestones/m5/features-v0.1.md | verify 2-VM failover for each deployment (dev-docs/release-checklist.md §3) |

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
| No command removes an enrolled member; no audited operator revocation on the primary for a root-key revocation it can never record | F28-HISTORY-RECHECK | owner decision |
| An unpinned Kubernetes node exits during an API-server outage instead of staying up and unready (G22-WP4 item 2); the fencing loop in `kube_cluster_test.go` counts a failed `/healthz` as not leader | QUAL-KUBE-UPGRADE (D373) | v0.2 |
| Data race between `TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones`'s `opts.Now` replacement and the integration host's refresh | HA-FIX | v0.2 |
| Paired benchmark and fixed-class run on the final head; PR body | G23, G25 | v0.2 |

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

| Item | Source | Target |
|---|---|---|
| Hosted CI is red on every pushed head after 68a709e2 (ee76c04b to 85913663). Most failures are sign-in or `POST /cluster/activate` answering `503 unavailable` right after a node reports ready: a command held to the publication it started on is refused when a background publication (a zone status write) lands first (D313). Seen in `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`, `TestRunServeDirectAnchorsFencedAndConflictRestart`, `TestStaticClusterProcessesReadOnlyPeerLossRestart`, `TestEnrolledClusterProcessesPromoteAndRejoin`, `TestAnchorUpgradeStaticTwinV010` and `TestV01BinaryRefusesEnrolledDirectory`. Fix is the integrator's D399 (CMD-RECAPTURE), not merged at 17b7c87a; CI-RED-3's quiet wait in `enrolled_conflict_test.go` is a stopgap to remove after it | hosted CI; CI-RED-2, CI-RED-3 verify | v0.2 |
| Hosted e2e `TestEnrolledManagedAnchorsPromoteAndRejoin` fails with "rejoin acquisitions 5" on f6afe7d6, b9b66cba, b6e09f9d, 3a6ab137 and 85913663 (the integrator's hand merge of the F28 and HA test flows); test-only data race in `TestAnchorEdgeActivationJoinedFollower` (`joinPrimaryOn` writes `p.opts.Config.Node.DataDir` at `cluster_join_test.go:51` on a running runtime) seen on 85913663 | hosted CI; F28-REJOIN-COUNT | v0.2 |
| Readiness under CPU starvation: the readiness budget of `startStaticMount` has no run that turns a failing condition into a pass (under the reproducing condition the test fails at its own 5 s HTTP timeout), the multi-P credit is not exercised, and the other mounted tests keep fixed 20 s readiness loops | CI-RED verify; FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| A UDP answer resolved from a serving generation that a republication replaces before the write is dropped silently (`serving: dependency changed`, `internal/dnsserver/native_capture.go`), so any settings or data commit can drop UDP answers in flight; resolve once more against the current generation or answer SERVFAIL, then tighten the refused-fenced check of `test/e2e/enrolled_cluster_test.go` | HA-OPEN verify | v0.2 |
| Back-to-back RFC 2136 UPDATEs through an enrolled follower: after the first SERVFAIL every later update of the burst fails (5 or 6 of 8 in the verifier's run, not applied); two of eight enrolled e2e tests failed locally at 71ac05f0 on this; the stale comment on `enrolledFollowerBurst` names the wrong cause | HA-UPDATE-ROUTING verify; F28-PROCESS merge | v0.2 |
| Through a follower, 84 of 200 new documents of one apply were refused with "the follower's source changed" or "cluster authority changed", four repeats created none, and repeats reported fewer documents unchanged than had been created | DOCS-RECONCILE implementer | v0.2 |
| Long multi-item request on SQLite: an unexplained 1.39 s DNS gap in about one run in a hundred without race (and one refused read in 1 of 15 runs on one CPU); under race on a loaded machine most of a second caller's reads are refused with `generation_pending`; the non-race run is a CI gate with thin margin; `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` fails about half the time without race on the loaded lab machine on its read assertion | CORE-APPLY-OPEN and CORE-OPEN3 verify; integrator | v0.2 |
| Status-write SERVFAIL fix: the silent-probe bound of `serve_status_write_test.go` has less headroom than its comment states (10 of 24 seen); the fixer's mounted and lint evidence is incomplete (`fix-03-mounted.log`, `fix-04-lint.log`) | CORE-STATUS-WRITE-SERVFAIL verify | v0.2 |
| Managed root anchors: F28-HA-4 is proven only by `TestSoakEnrolledAnchorsOfflineRecovery`, which skips without `KUDURRU_ANCHOR_SOAK=1` and passed once; refused-adoption and fenced status have no real-path or scraped-metric test; never-SERVFAIL tolerates SERVFAIL before a role's first validated answer; one undiagnosed run had a healed conflicted member answer SERVFAIL for its own zone (`dns_only`); a just-promoted or restarted managed primary answers writer commands 503 until its updater's first step; the capture-phase `ErrCAS` retry and the abandoned-term return have no test; F28-OPEN6 was backed out because `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart` then failed every run | F28-PROCESS, F28-EF1-LIVE, F28-HISTORY-RECHECK verify; integrator | v0.2 |
| Tier-B hooks and webhook records: the mounted 24 h expiry and budget-exhaustion cases, a mixed-version test for the IntegrationEventCursor kind and a negative run of the A1 audit rows are missing; with no Integration lease running nothing prunes; a hookless owner whose lease restarts more often than every 15 minutes never runs retention; deferred topics `query.blocked`, `vip.master_changed`, `client.seen`, `client.left`; no per-namespace event filter | F60C-HOOKS, F60C-OPEN | v0.2 (topics later) |
| Kubernetes integration host: K3 and K7 stay partial (two-replica budget sharing with hand-placed Leases only, credential rotation proven by order only, no holder-change, partition or mid-cleanup lease-loss qualification, expired license and re-created Integration proven at the adapter only); UniFi record publication and Tier-B webhooks are refused on Kubernetes; native Secret credential references are disabled; definite head-write rejections (403, 422) are reported as outcome unknown; the CLI `get` is not run on Kubernetes | F09B-KUBE-ROOT, F09B-KUBE-SOURCES, F09B-OPEN | v0.2 |
| Dashboard Integrations page: the `integrations` CLI and API are mounted (F60c), but the dashboard has no page to list, check, preview, sync or release an Integration; the Kubernetes refusals are in the row above | F60c | v0.2 |
| Zone groups: a zone deleted between `authorizeWrite`'s read and `writeObject`'s read is recreated by a group member's looping PUT without a cluster-scope check (code trace); a zone-group read error makes lists omit group zones with HTTP 200; the mounted tests retry the SQLite `503 unavailable` at reads, PUTs and sign-in. Impersonation: `impersonateVerbDenied` matches any 403 containing " cannot impersonate" | IAM-ZONEGROUP and IAM-IMPERSONATION verify (CORE-OPEN4) | v0.2 |
| Edge blocks: the five-minute restore hold measures wall time since the first lower sighting, so two pulls five minutes apart accept a stale revision at once; after a deliberate restore new blocks are delayed up to five minutes (not in the runbook); an owner whose only backend misses a pull has its blocks cleared and reinstalled | EDGE-DYNBLOCK verify | v0.2 |
| Known issue: the pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3 request whose QUIC FIN arrives in its own empty frame. The live client-ID test's canary fails once an image bump fixes it (then drop the retry and the known-issue text); the canary cannot tell the defect from another failure; `h3Post` leaks a goroutine on a dial failure; one DoQ loss was attributed to the defect by inference only | EDGE-CLIENT-ID | dnsdist upstream; after v0.2.0 |
| RPZ: the SQLite compile window after a restart was never measured; a signed feed restarted without the apiserver fails closed instead of reusing its persisted copy (D390); the fake API data and a golden file keep the old pending message; on Kubernetes the dig explanation and policy reads report no RPZ fields; with blocks on two ports answering one name, the CoreDNS import report still says the dropped block's name is imported and gets that block's answer on every listener, and the port-drop line counts omitted answers rather than names (RPZ-OPEN verify); a pending list of any kind, not only an RPZ one, makes a decision filter (RPZ-OPEN review nit) | RPZ-KUBE, RPZ-DIAG, RPZ-OPEN | v0.2 |
| Dashboard: every non-admin session polls `GET /cluster` every 30 s and gets an audited 403; no mounted follower test for QueryLogPage, ClientDetailPage, EntriesBrowser, the migration pages, ExternalUsers and RecoverAuthenticatorButton; no clean full vitest run at the follower fix head; authenticator and local-user controls are not hidden from the `/auth/status` capabilities on Kubernetes; no views for RPZ feeds, query-log sinks, edge blocks or zone-group scopes at this head | DASH-FOLLOWER verify; IAM-IMPERSONATION, RPZ-DIAG, OBS-OPEN | v0.2 |
| Settings after the first 0.2 start: `PreparedImage.CommittedDynamicOwnership` rebuilds the zone's dynamic index on every publication; `docs/deploy/runbooks.md` (near line 600) and `docs/deploy/compose-ha.md` (near line 108) say a later file edit of any cluster key is not applied, which is inaccurate for restart-required keys; static twins cannot change a cluster setting after their first 0.2 start; `TestAnchorUpgradeStaticTwinV010/LaterFileEditNotApplied` does not assert the seeded value | QUAL-OPEN2 and QUAL-MIXED-VERSION verify | v0.2 |
| Import step dispatch: the re-capture before a step's body (`57cef0e6`) refines the dispatch-once rule without a decision row (D399 generalizes it; not merged); a later import step is no longer bounded by 5 s | FLAKE-CLOCK-AND-READINESS verify | v0.2 |
| Evidence gaps: G21-PERMISSIONS `fix-bench01.log` and `fix-lint02.log` are empty; FLAKE-PORT-RACE's ledger points at unfinished logs (use `verify-01-mounted-race.log`, `verify-03-lint.log`); `dev-docs/design-record.md` still mentions `settings.operatorSeesQueryLog`, which does not exist; the permission scan's allowlist matches by bare name | G21-PERMISSIONS, FLAKE-PORT-RACE verify | v0.2 |
| Low-severity notes left open: HA-SELF (200 for any token authentication failure on the new-hash view; every seal error reported as `committed_output_unavailable`; no mounted follower audit test); FLAKE-PRIMARY-UNAVAILABLE (`RunnableStatus.LastError` drops an error at term end); OBS-TRACING (about 5 KB per traced query); CORE-RETAINED-SERVFAIL (stale `UpstreamContacted` comment; untested non-deadline refusal after contact); F09B-OPEN (joined budget error classification untested; timing-dependent bounds); QUAL-OPEN3 wording of the measured percentages | verify results in `followups/*.json` | v0.2 |
| Hosted job for the local-only interop suites: start a private dockerd on the runner and run `hack/test-knot-catalog.sh` (Knot `cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9`, knotd 3.6.2; `TestKnotCatalogConsumer` and `TestKnotInboundSecondaryTransfer`, last unskipped pass at `2e4e89fc`) and `hack/test-dnsdist-live.sh` (`powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`; all live dnsdist tests: F43 YAML/Lua, F28, F39b (both tests), anchor edge and F18c, last unskipped pass at `d2039002`, `out/QUAL-OPEN3`). Until then both are local-only evidence | QUAL-INTEROP, G23-R5; D383 | after v0.2.0 |
| Decide whether the secondary receiver should accept a signed IXFR reply that is one SOA at the client's serial without AA (Knot 3.6.2 sends it; BIND sets AA). Today such a reply is refused (`secondary: non-authoritative response`); a refresh against one consistent primary never asks for it, but it is reachable when the SOA poll and the IXFR reach different instances behind one address or the primary moves back to the stored serial between them. Effect: one refresh error, no renewed confirmation for that cycle, no data loss | QUAL-INTEROP review; `test/interop/knot_secondary_test.go` | v0.2 or later |
| Authoritative no-response window after a canonical commit. An authoritative UDP answer captured from a publication that a later commit supersedes is withheld (D313, D332), so the client gets no response until the next publication is installed. A DNSZone write also gives that zone fail-closed SERVFAIL coverage until its new lifecycle source is admitted. One zone write is several commits, and under `-race` on 2 shared CPUs the windows lasted up to about 1.3 s per commit (QUAL-OPEN3 `diag05`, `diag06`, `diag07`). Without `-race` and with `CGO_ENABLED=0` (mounted `runServe` test binary, 8 shared CPUs, `fix-measure03-release.log`): any canonical commit, even an unrelated BlockList write, left an authoritative zone unanswered for up to 586 ms; a write to the zone or its records gave SERVFAIL then no response for up to 860 ms; a forward zone gave SERVFAIL for up to 558 ms after its own write. dnsdist does not retry such a UDP query, so a client behind an edge sees a timeout, and a busy cluster that commits often loses that share of authoritative queries. Not changed for v0.2.0; shipping it as is needs the owner's explicit acceptance (D383). Next: repeat the measurement on the packaged release binary with a realistic object set; add a test that asserts an upper bound on the unanswered window after a commit (the live suites now wait it out, so no test bounds it: QUAL-OPEN3 `fix-measure-window.patch` is a starting point); keep the authoritative answer path from being invalidated by a commit that does not touch the zone. Then decide whether a withheld authoritative answer should become an empty SERVFAIL (as D332 already allows for unavailable local data), or whether the publisher should shorten the window, for example by keeping the admitted model of a zone whose only change is a query policy. | QUAL-OPEN3 (D383), live tests `TestRunServeRemoteEdgeAuthoritativeCacheACL` and `TestRunServeManagedEdgePinnedDaemonRestart` | after v0.2.0 |
