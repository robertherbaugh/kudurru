# v0.2.0 completion: integrator state (parent session scratch)

Started 2026-11-10 from 33ee04bc (PR #54, CI green on that head).
Maps: `map-*.json` here (10 read-only mappers). Brief for agents: `AGENT-BRIEF.md`.
Lane runner script (reuse with scriptPath + args):
`/home/vercel-sandbox/.claude/projects/-home-vercel-sandbox-kudurru/c8702f26-f312-4c72-b527-f2f104c61581/workflows/scripts/v020-lane-wf_a2ecd1ee-8f6.js`

## Protocol
- Lanes are persistent worktrees `.context/wt/<lane>` on `wip/v020/<lane>`.
- Agents commit source commits + one `chore(gen):` commit; write `out/<PKG>/{decision,ledger,changelog}.md`.
- Integrator: cherry-pick non-gen commits onto the PR branch, regenerate, splice
  decision rows (design-record), ledger sections (mirrored in completion-plan.md,
  gaps.md, followup-audit.md), changelog bullets; run composed checks; push; watch CI.
- Never commit `dashboard/dist/index.html` (built index; restore at the end with
  `git checkout -- dashboard/dist/index.html`).
- Docker: `sudo -n dockerd --host=unix:///tmp/kudurru-lab-docker.sock --data-root=/tmp/kudurru-lab-docker-data --exec-root=/tmp/kudurru-lab-docker-exec --pidfile=/tmp/kudurru-lab-docker.pid --storage-driver=overlay2 --bridge=none --iptables=false --ip-forward=false --ip-masq=false`
- Paired benchmark on final head: remove + re-add label `release-benchmark-paired` on PR 54.

## Decision numbers
- D367 Integration Delete lifecycle (INT-DEL-A/B/C)
- D368 Follower contract: forwarded domains + typed primary_required refusal (HA lane)
- D369 Managed anchors under enrolled HA (F28-FENCED, F28-HISTORY)
- D370 Managed anchors: kube refusal, image-less receiver, mixed-version position (F28-GUARDS)
- D371 V2-OBS delivery contract: per-sink queues, direct ClickHouse/Loki sinks (OBS-FANOUT, OBS-HTTP-SINKS)
- D372 dnstap raw override privacy contract (OBS-DNSTAP-EMIT)
- D373 G22 scope decisions: artifact definition, v0.1.0 baseline, mixed-version policy, legacy objects (QUAL lane)
- next free: D374

## Wave 1 (running)
- lane int  (task wgjhvn4wl): INT-DEL-A, INT-DEL-B, INT-DEL-C
- lane ha   (task wtuz9k63x): HA-FIX, HA-REFUSAL, HA-AUTH-SETTINGS
- lane f28  (task w1cn1hf8x): F28-FENCED, F28-HISTORY, F28-GUARDS
- lane obs  (task wveev7b3d): OBS-FANOUT, OBS-HTTP-SINKS, OBS-DNSTAP-EMIT
- lane qual (task wa0zbbkvg): QUAL-CI, QUAL-SQLITE-UPGRADE, QUAL-KUBE-UPGRADE
- lane docs (task wt6m2afs1): DOCS-ARCH, DOCS-OPS, DOCS-VERSION

## Backlog by lane (map ids)
- int: F60c-H1+R1; H2+H3; H4+A1; R2; R3+R4+C1; Q1+D1  -> then kube: F09b-K0..K8, DEL-8, F60c-K1
- ha: FWD-SELF (+ allow/deny domain commands, unblock decisions); UPD-1/G21-04; FWD-E2E+G21-11+FWD-DOCS
- f28: F28-HA-3+HA-4; F28-RFC-1/3/4 (+RFC-2); F28-GATE-4; F28-GATE-2 docs; F28-MV-2 (upgrade lane)
- obs: OBS-1 -> OBS-2, OBS-3, OBS-5; OBS-4
- iam: IAM-2 zonegroup; IAM-1 impersonation; IAM-3 writers API; IAM-4; G21-06; G21-07; G21-08 CLI
- edge/id: EDGE-1; ID-1; ID-2 API + DHCP MAC fill
- rpz/ops: RPZ-1; RPZ-2 API/CLI; OPS-1 CoreDNS; INT-6 group views; INT-4 external zones
- int2: INT-2 UniFi push/actions; INT-5 API (manifests, Integration writes)
- dash: integrations page, inventory view, RPZ UI, cluster wizard (HA-1), IAM writers UI, follower gating (FWD-UI), G21-09/10
- qual: G22-WP0..WP8; G23-R3/R4/R5/R6/R7; G24-SYNCTEST; ZONE-Q1; DNS-Q1
- docs: G21-13..17; G25-LEDGER/PACKAGES/CHANGELOG/PRBODY/ROADMAP/OPDOCS/FOLLOWUPS
- final: full local pass, hosted CI, paired benchmark, PR body

## Owner-only (not passed, record as such)
AD/Windows (D353), two-VM VIP (D358), licensing gates and promotion-time checklist
(G24-OWNER-GA), optional production-signed multi-node license run (G22-WP9).

## Status log
- 2026-11-10: credits ran out mid-wave; all six lane workflows resumed with resumeFromRunId
  (new task ids: int w87w0j6ml, ha wb0n83rlw, f28 w15schnfr, obs w50fvlco4, qual wkrkkhrxt, docs whqvopunv).
  Run ids: int wf_a2ecd1ee-8f6, ha wf_cfcc336f-97c, f28 wf_041745b1-6b7, obs wf_87691d0f-60b,
  qual wf_41515588-975, docs wf_4998fe66-758.
- Lane args for ha and obs carry interruptedFix (HA-FIX, OBS-FANOUT).

## Integration pointers (last lane commit already cherry-picked onto the PR branch, local only)
- docs: 51878320 (DOCS-ARCH complete; gen commit skipped, regeneration pending)
- int: 33ee04bc (nothing; Delete lifecycle A+B+C must land together)
- ha: 33ee04bc
- f28: 33ee04bc
- obs: 33ee04bc
- qual: 33ee04bc
- PR branch local head after DOCS-ARCH: 4e4b4bcf (not pushed; origin is 33ee04bc)

## Notes found during wave 1
- QUAL-CI found product defects: no answer for an upstream-failure SERVFAIL across a canonical commit
  (fixed in qual lane 49aae9ee); 503 completion_unknown on follower PUT when primary authority advances
  between commit and answer (ha lane 29cdf81a "acknowledge member commands under their own commit").
- INT-DEL-A alone leaves Delete integrations terminating; do not integrate without B and C.

- 2026-11-11: second credit cutoff + machine restart. Lane script now supports per-package models
  (implementers and fixers on opus, reviewers/verifiers inherit with opus fallback), interruptedImpl /
  interruptedFix2 notes, and stops a lane when an agent dies. Heavy wrapper now has two slots.
  Resumed task ids: int wnih6kteq, ha w9t6yf0t8, f28 wf3bihlmh, obs wys1rftok, qual wlgirgtq9, docs wnsy63f6n.
- Integrated locally onto the PR branch (head b4987f32 before regeneration): DOCS-ARCH, QUAL-CI, HA-FIX,
  F28-FENCED, OBS-FANOUT, DOCS-OPS. Pointers: docs 5f10ed88, qual 0048c606, ha ba1351ac, f28 f9818a59,
  obs 7c4e1779, int 33ee04bc (Delete A+B+C must land together).
- Decision numbers assigned at integration: D374 kube answers for forwarded auth routes (HA-FIX row 1),
  D375 multi-item requests through a follower (HA-FIX row 2), D376 member command acknowledgment under
  the commit's writer exclusion (HA-FIX row 3), D377 retained SERVFAIL across a canonical commit
  (QUAL-CI row; CORE-RETAINED-SERVFAIL rewrites it), D378 multi-document requests on the complete SQLite
  runtime (CORE-APPLY-MULTI). Next free: D379.
- New lane core (wip/v020/core from b4987f32): CORE-APPLY-MULTI, CORE-RETAINED-SERVFAIL
  (spec: map-core-defects.json). Found by verifiers: multi-document apply applies one document per run on
  SQLite (base defect); 49aae9ee releases node-produced SERVFAILs on recursive nodes after each commit.
- Ledger/decision/changelog splicing for integrated packages is still pending (out/<PKG>/ files).

- 2026-11-11: first batch pushed. PR branch origin = de0d249a (62 commits over 33ee04bc, including
  chore(gen) de0d249a). Pre-push evidence: evidence/INTEGRATE-1/ (prepush02.summary: gofmt clean, build,
  vet, drift, lint 0 issues, 17 library packages race ok, cmd new-test selection ok; dashboard.log
  typecheck + 50 tests). First attempt prepush01 did not run (script not executable); retained.
  Hosted runs on de0d249a: ci 42954844334, security 42954844450 (watcher task b1rlr2j20 writes
  evidence/INTEGRATE-1/hosted-de0d249a-runs.json).
- Known open in the pushed tree: 49aae9ee over-broad retained SERVFAIL (lane core fixes);
  multi-document apply defect (lane core); ledger/decision/changelog splicing pending.
- Lesson: never trust a background command's exit code when the command line ends in cat/tail/echo;
  read the per-step logs.

- 2026-11-11: docs lane finished its tranche (11 agents, no errors). DOCS-VERSION (66c45c13..a10afdf1 on
  wip/v020/docs, base 5f10ed88) is NOT integrated yet: verify left 1 major unresolved + 4 minor/nit, and
  its pages now warn about three product defects that other lanes are fixing:
    1. unserved DNSZone type stops a SQLite node  -> fixed in qual lane QUAL-SQLITE-UPGRADE (c65a2f24..)
    2. one apply run applies one new document     -> core lane CORE-APPLY-MULTI
    3. never-downloaded BlockList URL stops a node -> fixed in qual lane (8a7ee71b)
  Integrate DOCS-VERSION only together with a DOCS-RECONCILE package (docs lane, after qual + core + ha land):
  remove the defect warnings that are no longer true; fix verify:DOCS-VERSION new problems (offline-config.md
  --select-object advice, 'minutes apart' backoff wording, prepare_failed/preparing, catch-all CIDR vs client ID);
  fix verify:DOCS-OPS new problems (dynamic-updates.md 'Check four things' is incomplete: three more policy
  shapes stop a zone; sqlite-cluster.md section 4 contradicts sqlite-join.md#commands-through-a-follower);
  G21-13 deployment-shape matrix + error-key glossary; decide 319529fc (enterprise page disagrees with
  website/src/content.ts; website/ needs owner authorization, so do not edit it: drop or reword the commit).
- External check: 'Workers Builds: kudurru-www' failed instantly on de0d249a (not present on 33ee04bc);
  same zero-duration failure mode as the excluded kudurru-docs check; reported to the owner, not resolved.

- 2026-11-11: hosted result for de0d249a: 9 workflows success; ci 42954844334 FAILED in 'go test 1/2'
  (20 other jobs incl. e2e, go test 2/2, lint passed). Log: evidence/INTEGRATE-1/hosted-de0d249a-gotest1.log.
    a) TestRunServeQuerylogFailedPrepareKeepsOldCompleteRoute: 'ops: listen tcp 127.0.0.1:42457: bind:
       address already in use' = harness free-port race (offlineFreeAddress listen/close then bind). Same
       class seen before the batch (hosted 73d4b057, TestOfflineAnchorRecoveredTrustServesAfterRestart).
       TODO package: fix the harness port race (qual lane) instead of relying on reruns.
    b) TestCLIOIDCBrowserActualTLSProofRefusals/nonce: 'browser writer ended context canceled'
       (RunWriter returned context.Canceled while the test ctx was alive). NOT found in any earlier retained
       hosted log -> cannot be called a known flake. Batch touched anchor supervisor/updater, querylog
       runtime, member objects, output guard. Repro running: 30x race on head vs base (task bjb8s4lq7,
       logs evidence/INTEGRATE-1/oidc-repro-*.log); hosted attempt 2 of the failed jobs requested.
       Control worktree: .context/wt/base-33ee04bc (remove when done).
  Do not report the batch as green on a passing rerun alone.

- 2026-11-11: OIDC repro on head: 1 failure in 30 (subtest multi, same message). Base control INVALID
  (shell precedence bug ran the whole cmd suite x30 outside the wrapper on CPUs 4-7 from about 04:40 to
  05:40 UTC; may have disturbed lanes' timing-sensitive tests in that window; log kept as
  evidence/INTEGRATE-1/oidc-repro-base-INVALID-ran-whole-suite.log).
  Hosted attempt 2 of ci 42954844334 failed too, on a different test:
  TestServingHTTPClusterActivationIdentityRefusalAndRollback/invalid-member 'primaryruntime: required
  adapter/publication unavailable' (class seen pre-batch on ffd18d41, b2d197d8). Pre-batch baseline:
  19bfecc4 (docs-only) failed the same shard on TestServingHTTPAuthCookieFactorAndPasswordClears/confirm
  (401 vs 204) and TestSupervisorLifecycle (ee/edge/dnsdist). 8 of the last 12 completed ci runs failed.
- New lane flake (wip/v020/flake from de0d249a; spec map-flakes.json): FLAKE-OIDC-WRITER,
  FLAKE-PORT-RACE, FLAKE-PRIMARY-UNAVAILABLE, FLAKE-AUTH-COOKIE-AND-SUPERVISOR.
  Nothing more is pushed until FLAKE-OIDC-WRITER says whether the batch introduced the writer failure.

- 2026-11-11: f28 lane tranche 1 finished (run wf_041745b1-6b7, 11 agents, no agent errors). NOT integrated
  beyond F28-FENCED (pointer f9818a59; lane head 43092a90):
    F28-HISTORY: impl done, review no blocker/major, fix 8a047222 changed production behavior (refuse adoption
      while a carried revocation is pending) and was never verified -> package F28-HISTORY-RECHECK.
    F28-GUARDS: partial. Major UNRESOLVED: F28-EF-1 live dnsdist follower test not written (no Docker then)
      -> package F28-EF1-LIVE. Note: anchorMode managed on a non-sqlite driver now fails validation (D370).
  f28 tranche 2a launched (task wv9puwv7d, run wf_cd2fbd5f-e3e): F28-HISTORY-RECHECK, F28-EF1-LIVE,
  F28-PROCESS. Backlog 2b: F28-STATUS (GATE-4, must show carried denial / refused adoption / fenced reason),
  F28-RFC (RFC-1/2/3/4, decision D379 for timers), F28-DOCS (GATE-2).
- Private Docker daemon started by the integrator (socket /tmp/kudurru-lab-docker.sock, log
  /tmp/kudurru-lab-dockerd.log); brief section 5 documents its rules. Pinned dnsdist image present.
- Lane script: packages with verifyFix:true get a verification pass whenever the fixer committed anything
  (use on all new packages). Post-verify of already integrated, unverified fix commits: OBS-FANOUT
  (task wrkbdqk41, run wf_e35d6a8c-451; script v020-post-verify-wf_e35d6a8c-451.js).
- Running lanes: int wnih6kteq, ha w9t6yf0t8, obs wys1rftok, qual wlgirgtq9, core wgecuqh47, flake wf2he2mdk,
  f28 wv9puwv7d. docs lane idle (DOCS-RECONCILE waits for qual + core + ha).
- PUSH HOLD until FLAKE-OIDC-WRITER reports whether the first batch introduced the writer failure.

- 2026-11-11 (later): the Claude Code process ended and stopped all eight workstreams (machine not rebooted).
  OWNER: ignore both Cloudflare 'Workers Builds' checks; ensure all changes are committed and pushed.
  => No push hold any more: integrate verified packages onto the PR branch as they finish, and keep
  everything else on the remote under wip/v020/*.
  Remote backup (verified with ls-remote): wip/v020/{int,ha,f28,obs,qual,docs,core,flake} = lane branches;
  wip/v020/{ha,core,flake}-uncommitted = snapshot commits of dirty worktrees (lane branches/worktrees
  untouched); wip/v020/handoff-notes = PLAN, brief, maps, out/ drafts, scripts (no evidence logs).
  backup.sh loop 900 runs detached (log backup.log) and force-updates only refs under wip/v020/.
- Package state at the stop (journals): nothing new is ready for the PR branch.
    int: INT-DEL-A verified; INT-DEL-B verified but 5 new problems (followups/INT-DEL-B-verify.json);
         INT-DEL-C impl interrupted after 5 commits (clean tree). Delete must land as A+B+C.
    ha:  HA-REFUSAL impl done, review 0 major/5 minor/4 weakened, fix PARTIAL, never verified
         (followups/HA-REFUSAL-unverified.json); HA-AUTH-SETTINGS impl interrupted (1 dirty file).
    obs: OBS-HTTP-SINKS impl done, review 0 major/5 minor, fix done, never verified
         (followups/OBS-HTTP-SINKS-unverified.json); OBS-DNSTAP-EMIT impl interrupted (clean tree).
    qual: QUAL-SQLITE-UPGRADE review had 6 major; after fix verify left 2 UNRESOLVED (one blocker class:
         record-set endpoints the zone builder rejects still stop a zone after upgrade for other shapes)
         + 4 new (update grants refused by the primary runtime; a list URL that never answers takes a node
         down until the fetch ends; TestUpgradeFromV01DataDirectory flaky 2/7; missing test shape)
         -> followups/QUAL-SQLITE-UPGRADE-verify.json, new package QUAL-SQLITE-UPGRADE-2.
         QUAL-KUBE-UPGRADE barely started.
    core: CORE-APPLY-MULTI impl done (9 commits), review 1 major/6 minor/5 weakened, fix interrupted (dirty).
    flake: FLAKE-OIDC-WRITER impl interrupted (dirty: test files + serving_fixture_teardown_test.go).
    f28 tranche 2a: F28-HISTORY-RECHECK interrupted at start. post-verify OBS-FANOUT interrupted.
  Post-verify still owed (run when the lane worktree is idle): HA-REFUSAL, OBS-HTTP-SINKS.

- Resumed after the process restart (same run ids): int wrmm54e5u (INT-DEL-C now also resolves the INT-DEL-B
  verify follow-ups, verifyFix), ha wkocc8x2j, obs wadbj8iuy, qual wppjh5gph (new package
  QUAL-SQLITE-UPGRADE-2 inserted before QUAL-KUBE-UPGRADE), core w509bzptn, flake wntj88non,
  f28 tranche 2a w0z6m7gjg (run wf_cd2fbd5f-e3e), post-verify OBS-FANOUT wbp6snjus.
  Resume args must repeat: ha interruptedFix [HA-FIX] + interruptedImpl [HA-REFUSAL, HA-AUTH-SETTINGS];
  obs interruptedFix [OBS-FANOUT] + interruptedImpl [OBS-HTTP-SINKS, OBS-DNSTAP-EMIT]; qual interruptedImpl
  [QUAL-SQLITE-UPGRADE, QUAL-KUBE-UPGRADE]; int interruptedFix2 [INT-DEL-B] + interruptedImpl [INT-DEL-C];
  core interruptedFix2 [CORE-APPLY-MULTI]; flake interruptedImpl [FLAKE-OIDC-WRITER]; f28 2a interruptedImpl
  [F28-HISTORY-RECHECK]; plus the same per-package models as before (cached prompts depend on them).

- Post-verify OBS-FANOUT (wbp6snjus): verdict sound; findings 1,3,4,5 resolved (tests fail on pre-fix code,
  no races). Open: (a) 300 ms close bound in TestQuerylogRuntimeSinksLegacyReload too tight -> widened to
  2 s by the integrator on the PR branch (uncommitted until its 12x race run on 2 cores passes, task
  by6x9khr0, log evidence/INTEGRATE-1/sink-close-bound01.log); (b) SERVFAIL servfail_unsynced window on a
  primary zone after a status write -> new spec CORE-STATUS-WRITE-SERVFAIL in map-core-defects.json for the
  core lane's next tranche (base control not yet run by anyone but the fixer); (c) two nits in
  followups/OBS-FANOUT-postverify.json for the next obs package.

- PR branch pushed at bb6b73aa (test(querylog): bound the sink shutdown check above the local close);
  12x race run on 2 cores passed (evidence/INTEGRATE-1/sink-close-bound01.log: ok 61.9 s, exit=0).
  Lesson again: 'exit code 1: No matches found' on that task was the trailing grep -c, not the test.

- OWNER (2026-11-11): merge all wip branches into PR 54, then validate what is left. Done: all eight lanes
  cherry-picked (pointers = lane heads: qual 2fad1dad, core 02089768, ha 17246f3a, int 0fac45e0, obs d4e63712,
  f28 90740ec6, flake 796c04e3, docs a10afdf1), regenerated, pushed. Evidence evidence/INTEGRATE-2/.
  Pre-push: build/vet/drift ok; lint + 3 tests fixed (release route follower class, counts 49->50, errcheck);
  host lease_cancel passed 3/3 on rerun; LongApply and UpgradeWithLegacyPlaceholders passed alone
  (rerun-two.log) but LongApply shows an 80 s DNS gap and 41 of 43 reads failed during a 120-document apply.
  Full lint/libs/cmd were NOT rerun after the fixes. Credits ran out again; no lanes running.

- 2026-11-11 17:45 UTC: machine rebooted again; diskguard, backup loop and dockerd restarted; build cache
  trimmed (34 GB free). Hosted ci 43045349810 on 68a709e2 FAILED: helm job TestChartUpgradeFromV01
  (`git archive v0.1.0` exit 128: no tag in the hosted checkout) and go test 1/2
  TestServingBlockingWriterCanceledAfterCommitBeforeObservation (`providerresources: owner closed`).
- Wave 2 (all lane worktrees reset to 68a709e2; specs map-wave2.json; pointers = 68a709e2 for every lane):
    flake wruevxdk8 (wf_73c161b7-717): CI-RED, FLAKE-OIDC-WRITER, FLAKE-PORT-RACE, FLAKE-PRIMARY-UNAVAILABLE
    core  wdyqrru1j (wf_8ace7888-d99): CORE-APPLY-OPEN, CORE-RETAINED-SERVFAIL, CORE-STATUS-WRITE-SERVFAIL
    qual  wd55ih7m2 (wf_313bf638-45d): QUAL-UPGRADE-OPEN, QUAL-MIXED-VERSION
    f28   wh453cjq3 (wf_c68574a2-fd6): F28-HISTORY-RECHECK, F28-EF1-LIVE, F28-PROCESS
    docs  wg5mmlpmx (wf_520affdb-79f): LEDGER-SPLICE (may edit tracker/decision log/changelog), DOCS-RECONCILE
    int   w3kzuxha0 (wf_33785f2b-2d5): INT-OPEN, F60C-HOOKS (D379), F60C-RECORDS (D380)
    ha    w5ljbxcuu (wf_8b67c320-84d): HA-OPEN, HA-UPDATE-ROUTING (D381), HA-SELF
    obs   wymvh0a4b (wf_cc264be9-3fb): OBS-OPEN, OBS-DNSTAP-LISTEN, OBS-TRACING (D382)
  Next free decision number: D383. Resume with the same args (in the launch calls) + resumeFromRunId.
  Still unstarted: F09b Kubernetes host, IAM, edge/identity, RPZ on Kubernetes, CoreDNS importer, UniFi push,
  external zones, dashboard pages, F28-RFC, G23 benchmark/Knot, final G24/G25 pass.
- LEDGER-SPLICE integrated and pushed (5ab4b96c); docs pointer 64bb954f. Wave 2 lanes resumed 19:10 UTC after another process restart (same run ids; interruptedImpl set for the in-flight package of each lane, interruptedFix2 [INT-OPEN] for int).
- DOCS-RECONCILE integrated and pushed (ab661718); docs pointer dc3ff846; docs lane idle. Its implementer saw 84 of 200 new documents refused through a follower ('source changed'/'cluster authority changed'), repeats creating none: product defect for the ha lane.
obs pointer b69c545a
- OBS-OPEN integrated + regen + credential test order fix, pushed 8d9e976e. Wave 2 lanes resumed ~21:57 UTC (same args, added interrupted flags; see launch calls).
- 2026-11-12 01:30 UTC lanes resumed again (process exit at ~00:56). CI-RED integrated (flake pointer d65d0446) and pushed ebe67588; its verify left open: readiness stalls in TestRunServeStaticTwinsReadOnlyPeerLossRestart and TestServingImportSettingsSynchronousAndBackground; batches not all rerun. obs pointer b69c545a.
- 2026-11-12 ~02:30 UTC: OWNER: make sure everything is in the PR and keep working. All committed lane work
  cherry-picked (pointers: qual fd41cadb, core 56637cec, ha b2b20460, int 3dc7b342, obs 406d0091, f28 b1bfc6f0,
  flake d65d0446, docs dc3ff846), regenerated, prepush all green (evidence/INTEGRATE-4: 31 lib packages ok,
  cmd new-test selection ok 256 s, lint 0), pushed ee76c04b. Docs conflicts from the core lane
  resolved in favor of the already merged DOCS-RECONCILE text. Process exits when the integrator's turn ends:
  keep the turn open while lanes run. Lanes resumed 01:35 UTC (same run ids and args as the 01:27 resume).
- 02:45 UTC: FLAKE-OIDC-WRITER impl: fixture-only cause, base fails identically, first batch did NOT introduce it
  (in review). qual lane finished QUAL-UPGRADE-OPEN + QUAL-MIXED-VERSION (open items in
  followups/QUAL-wave2-open.json); qual pointer 8ca9b338 (local, unpushed commit 64a9ed2b). New qual run
  w6846fgud (wf_7364d7de-c42): QUAL-OPEN2, QUAL-INTEROP (D383). Next free decision: D384.
int pointer de2fe451
- 03:00 UTC: int lane finished wave-2 tranche (open: followups/F60C-wave2-open.json); new int run wd02jy0uq (wf_e50c73a1-6af): F60C-OPEN, F09B-KUBE-ROOT (D384), F09B-KUBE-SOURCES. Next free decision: D385. Local unpushed commits: qual docs + int tail.
- 03:20 UTC: hosted ci 43138800781 on ee76c04b: 20/22 jobs ok; only failure TestSupervisorLifecycle (ee/edge/dnsdist, baseline flake). Pushed cca19943 (qual docs + int tail; ee/integration race ok, Delete mounted selection ok).
- 03:30 UTC: core lane finished (long-apply DNS gap 80 s -> <1 s; open items followups/CORE-wave2-open.json);
  core pointer e59af9be; flake pointer 259540a5 (FLAKE-OIDC-WRITER verified clean). New core run we1exjxzj
  (wf_38d46ff0-269): CORE-OPEN3, IAM-ZONEGROUP (D385), IAM-IMPERSONATION (D386, D387). Next free: D388.
- pushed 7648c479 (core tail + OIDC fixture fix; prepush INTEGRATE-5 all exit 0)
int pointer 0a155110
obs pointer 30b43308
- 04:45 UTC: obs lane finished DNSTAP-LISTEN (1 minor open) and TRACING (verified, 4 low notes); open items
  followups/OBS-wave2-open.json. New obs run wrm0543zt (wf_6101a4b6-076): OBS-OPEN4, EDGE-DYNBLOCK (D388),
  EDGE-CLIENT-ID (D389). Next free decision: D390. Local PR branch holds F60C-OPEN + OBS-TRACING unpushed
  (regen + prepush INTEGRATE-6 pending).
- 05:15 UTC: hosted ci 43150328589 on 7648c479 FAILED go test 1/2 + e2e: TestRunServeDirectAnchors{BootstrapTransportsRestartAndDisabled,
  FencedAndConflictRestart} (login 503), TestRunServeSQLiteReversePTRPolicy, TestSupervisorLifecycle, e2e
  TestEnrolledClusterProcessesPromoteAndRejoin (activate 409). Only 16 lines of production code differ from ee76c04b
  (which failed only the supervisor test once): cause unknown, not attributed. Spec CI-RED-2 in map-wave2.json.
- ha lane finished HA-OPEN (1 open), HA-UPDATE-ROUTING (1 open), HA-SELF (verified); open items
  followups/HA-wave2-open.json. ha head 923d7720 NOT yet integrated (pointer b2b20460; integrate after prepush
  INTEGRATE-6). New ha run wdn409wyb (wf_0cf9590a-4f6): CI-RED-2, HA-OPEN5.
ha pointer 923d7720
- 06:30 UTC: pushed 518c5899: F60C-OPEN, OBS-TRACING, HA wave-2 tail (pointer 923d7720), regen, and
  the settings schema test update (tracing added a 5th upstream-group field; the lane had not run that test).
  prepush INTEGRATE-6: first run PREPUSH_RC=1 only on TestSettingsSchemaMetadata (drift + libs); reruns after the
  fix exit 0 (drift-rerun.log, api-rerun.log). Integrator now has its own heavy slot (/heavy-int,
  HEAVY=... prepush.sh) because the two shared slots starved the checks for 20+ minutes.
flake pointer 111982d5
- 07:00 UTC: pushed 2b8a5150: FLAKE-PORT-RACE (testport package; verified unres=0). Prepush INTEGRATE-7 exit 0
  but its cmd step ran no new tests; real evidence is batch-replay.log (hosted batch 2 of 7, 94 tests, race, 4 CPUs,
  BATCH_RC=0, no bind collisions). NOTE: prepush 'cmd' only covers tests added since base: for harness-wide or
  product-wide changes also replay a hosted batch (evidence/INTEGRATE-1/batch-oidc.txt, batch-querylog.txt).
- 07:40 UTC: hosted ci 43164920466 on 2b8a5150 failed fast: (a) go test 2/2 could not bind host port 55432 for its
  postgres service container at job setup (runner side, no repo code ran); (b) go test 1/2
  hack/test_bench_socket_calibration.py: FLAKE-PORT-RACE edited the digest-pinned workload file
  internal/dnsserver/quic_socket_test.go. Restored that file and pushed 2133f47e. prepush.sh now runs the hack/
  python guards first (step pyguards). Tell future agents: never edit files listed in
  bench/calibration/*/provenance.json workload_files.
- heavy wrapper raised to four slots at 06:35 UTC (two slots were held by long low-CPU jobs, load 2/8, lanes queued 25-43 min)
core pointer 3b922541 (CORE-OPEN3: open = unexplained 1.39 s DNS gap ~1/100 behind the 1 s gate in the no-race long-apply test; reads refused under race)
- 07:40 UTC: hosted ci 43167753755 on 2133f47e: ONLY failure TestSupervisorLifecycle (ee/edge/dnsdist; 3 of the last 4
  complete runs; pre-pass baseline failure; owned by FLAKE-PRIMARY-UNAVAILABLE, in progress). All cmd batches,
  go test 2/2, e2e, helm, python guards passed. The anchor-login/ReversePTR/e2e-activate failures of 7648c479 did
  not recur (CI-RED-2 still investigating them).
- pushed e31c1066: CORE-OPEN3 (prepush INTEGRATE-8 exit 0 + batch-querylog replay BATCH_RC=0).
obs pointer e8afae3a
- 08:00 UTC: pushed 38e640fa: OBS-OPEN4 + tracing cache test wait (30/30 race, 4 CPUs); prepush INTEGRATE-9 exit 0, batch replay ok.
- 2026-11-12 08:50 UTC INTEGRATE-10 pushed: qual 8ca9b338..d657cbfb (QUAL-OPEN2; verify unres=2,new=4, in
  followups/wave3-open.json; I fixed the two failing over-file warning tests on the PR branch) and flake
  111982d5..47d8fff5 (FLAKE-PRIMARY-UNAVAILABLE impl; review still running). qual pointer d657cbfb,
  flake pointer 47d8fff5. f28 b1bfc6f0 still (F28-PROCESS impl running, commits up to 593b5449 not in).
  Open doc wording: runbooks.md:602 / compose-ha.md:109 "file edit is not applied" is inaccurate for
  restart-required cluster keys (unverified claim from the verifier; check serve.go:537,:1248).
- 2026-11-12 09:30 UTC pushed 9c34e948 (QUAL-INTEROP, qual pointer de8d3cdb) and 1d80d456 (F09B-KUBE-ROOT,
  int pointer 66ec734d; verify unres=1,new=4 incl. a hot retry loop when the refusal status write keeps
  failing and credential fingerprints on refused foreign-namespace Integrations: queue as F09B-OPEN after
  F09B-KUBE-SOURCES). Cherry-picked flake 47d8fff5..2b073574 (flake pointer 2b073574).
  New runs: qual wf_462f2f1b-255 (task wm80v06ij): QUAL-OPEN3 (D383), RPZ-KUBE (D390), RPZ-DIAG (D391),
  OPS-COREDNS (D392). flake wf_389d475f-2af (task waaml2vxx): FLAKE-CLOCK-AND-READINESS, G21-PERMISSIONS
  (D393), G21-TSIG-CLI (D394), G21-REFUSAL-INVENTORY. Next free decision: D395. All verify results of this
  wave are in followups/wave3-open.json.
- 2026-11-12 10:50 UTC hosted ci 43192717720 on 291c55a5: 20/21 jobs green; go test 1/2 failed only
  TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled (login 503 after start; CI-RED-2 family;
  log evidence/INTEGRATE-13/hosted-291c55a5-gotest1.log). TestSupervisorLifecycle fixed.
  Pushed ecd633b1: IAM-ZONEGROUP (core pointer fcaa7d63; verify unres=0,new=5 incl. zone recreate race in
  writeObject -> queue CORE-OPEN4) and EDGE-DYNBLOCK (obs pointer 25beaebe; conflicts resolved: both
  runnables in serve.go, follower counts 33/14/49, helper startLicensedMount kept). INTEGRATE-15 libs
  failure was a cache trim during build; kube store rerun ok.
  QUAL-OPEN3 d2039002 conflicts with the restructured serve_anchor_edge_live_test.go: being ported by an
  agent in .context/wt/port (branch wip/v020/port); cherry-pick its commit, then remove that worktree.
  QUAL-OPEN3 finding for the owner: any canonical commit leaves authoritative zones unanswered up to
  ~0.6 s (D383 draft clause 5; out/QUAL-OPEN3/). Queue a core fix package (CORE-COMMIT-GAP).
  Do not trim go-build cache while a build runs (or accept a rerun). Push cancels the running hosted ci:
  leave ~75 min between pushes when a result is wanted.
- 2026-11-13 03:05 UTC: all lanes died on the weekly limit at ~12:30 UTC Nov 12; machine restarted; owner
  reset the limit. diskguard, backup loop, dockerd restarted. Pushed f6afe7d6 (INTEGRATE-16..18:
  IAM-IMPERSONATION, QUAL-OPEN3 port, F28-PROCESS [2 of 8 enrolled e2e failed locally on follower UPDATE
  SERVFAIL, HA-OPEN5 item], EDGE-CLIENT-ID, RPZ-KUBE, FLAKE-CLOCK-AND-READINESS).
  Pointers: core cd0ee44f, qual cf753155, flake 5d01c0cb, f28 97c9117b, obs 34224330 (lane finished),
  int 66ec734d (F09B-KUBE-SOURCES commits up to 08e35675 NOT in: verify pending), ha 923d7720 (CI-RED-2
  commits NOT in: fix interrupted), ci 2943cd6b (CI-RED-3 not in), dash f6afe7d6 (new lane).
  Hosted ci on ecd633b1 failed 5 tests (follower count fixed by me; rest = CI-RED-3 package).
  Resumed runs (same run ids; task ids): flake wqvkwnm9o [interruptedImpl G21-PERMISSIONS], core w4jcwwlnc
  wf_699a2628-7bc [interruptedImpl CORE-COMMIT-GAP; then CORE-OPEN4 D385, IAM-WRITERS D396, ID-INVENTORY
  D397], qual w2bu7yvz8 [interruptedImpl RPZ-DIAG], f28 w5wxjhj0d wf_3361f5f4-71c [interruptedImpl F28-OPEN6;
  then F28-RFC D398], ha w9kw3gaob [interruptedFix CI-RED-2], ci w00oow8f4 wf_ef55df25-fb4 [interruptedImpl
  CI-RED-3], int w35ap2479 [+ appended package F09B-OPEN D384], dash wceypkv1n wf_5b1df647-f66
  (DASH-FOLLOWER, DASH-CLUSTER, DASH-OPS). Next free decision: D399.
  Still unstarted: INT-2 UniFi push, INT-4 external zones, INT-5 integration admin API, INT-6 ClientGroup
  views, dashboard pages for integrations/inventory/RPZ/IAM writers, second LEDGER-SPLICE, final G22-G25.
- 2026-11-13 04:20 UTC pushed (INTEGRATE-19, 20): F09B-KUBE-SOURCES (int pointer 08e35675; verify unres=0,new=3)
  and G21-PERMISSIONS (flake pointer 0c068521; verify unres=0,new=6; access.md conflict merged by hand).
  Hosted ci 43355496597 on f6afe7d6 failed 4 tests (logs evidence/INTEGRATE-18/ci-f6afe7d6-*.log):
  e2e TestEnrolledClusterProcessesPromoteAndRejoin (activate 503 unavailable), e2e
  TestStaticClusterProcessesReadOnlyPeerLossRestart (login 503 unavailable), e2e
  TestEnrolledManagedAnchorsPromoteAndRejoin ("rejoin acquisitions 5": count bound, f28 lane),
  cmd TestRunServeDirectAnchorsFencedAndConflictRestart (CI-RED-2 fix not merged yet).
  The 503-right-after-ready family is the main hosted CI blocker: a command held to the publication it
  started on is refused when a background publication (zone status write) lands first (D313). CI-RED-2
  reorders tests; CORE-OPEN4 item 3 is to fix the product (re-capture before dispatch, as 28468b1e did for
  import steps). If CORE-OPEN4 does not close it, make it its own package.
- 2026-11-13 05:50 UTC pushed b6e09f9d (RPZ-DIAG; qual pointer 1f5c9179). Hosted ci on b9b66cba failed again
  on the 503-after-ready family (evidence/INTEGRATE-20/ci-b9b66cba-*.log) + "rejoin acquisitions 5".
  INTEGRATOR DECISION D399 (flag to owner in the final report): a command refused before its transaction
  began only because its captured publication was superseded captures again, re-checks everything and
  dispatches once, bounded. Package CMD-RECAPTURE in ci lane: wf_551cef7b-6d0, task wep10pdp2 (ci worktree
  reset to e4e0a059). TODO when verify:CORE-COMMIT-GAP lands: stop core run w4jcwwlnc and resume with
  CORE-OPEN4 notes minus item (3) (now CMD-RECAPTURE's), to avoid two lanes changing the command path.
  Local (unpushed until ci on b6e09f9d reports or ~06:50): CI-RED-3 (ci pointer 2ee92e93; item 4 open ->
  CMD-RECAPTURE) and DASH-FOLLOWER (dash pointer f0cdcb7c; verify unres=1,new=4: /cluster polled every
  30 s by non-admin sessions = audited 403 spam -> fix in a DASH-OPEN package).
  Next free decision: D400.
- 2026-11-13 06:40 UTC pushed 3a6ab137 (INTEGRATE-22, 23): CI-RED-3, DASH-FOLLOWER, G21-TSIG-CLI (flake pointer
  613107d5; verify clean). Known at push: dashboard AccessPage audit-log paging test times out locally
  (26 s vs 20 s limit) WITH the follower change and passes without it on a lane tree under the same load
  -> render cost regression to fix in a DASH-OPEN package (with the /cluster poll 403 spam).
  Hosted ci on b6e09f9d: login 503 (CMD-RECAPTURE), stored TSIG test (fixed by CI-RED-3, now pushed),
  activate 409 (CI-RED-2 fix, ha lane, not merged), "rejoin acquisitions 5" (3rd run in a row; result of my
  hand merge of F28 + HA test flows) -> package F28-REJOIN-COUNT on the obs lane (reset to 3a6ab137):
  wf_bf59fb86-dff, task wqnd8dcjc.
- 2026-11-13 06:55 UTC F09B-OPEN merged locally (verify clean; int worktree reset to 45160534). New int run
  wf_f322c5a1-34d (task wkzh1p8ps): INT-ADMIN-API D400, INT-UNIFI-PUSH D401, INT-EXTERNAL-ZONES D402,
  INT-GROUP-VIEWS D403. Next free decision: D404.
- 2026-11-13 07:45 UTC OPS-COREDNS merged locally (verify clean, new=4 -> RPZ-OPEN item 4). qual worktree reset
  to 85913663; new qual run wf_d804fb74-c94 (task w5js1qwrc): RPZ-OPEN (D390; spec map-wave3.json), then
  LEDGER-SPLICE-2 (spec map-wave3.json; documents what is at the lane head only; a third splice is needed at
  the end for later packages).
- 2026-11-13 08:05 UTC pushed 85913663 (F09B-OPEN, OPS-COREDNS). Hosted ci on 3a6ab137: ONLY e2e
  TestEnrolledManagedAnchorsPromoteAndRejoin failed ("rejoin acquisitions 5"); all other jobs green incl.
  dashboard (the local AccessPage timeout is load).
  core run stopped and resumed (task wxyf2xdwy, same run id) with CORE-OPEN4 reworded: item 3 (503s) belongs
  to CMD-RECAPTURE; added impersonation match + CORE-COMMIT-GAP leftovers. interruptedImpl now
  [CORE-COMMIT-GAP, CORE-OPEN4] (repeat exactly on any resume).
  Merged locally (INTEGRATE-26, checks running): CORE-COMMIT-GAP (D395; core pointer b1ae724a; verify
  unres=1,new=4) and F28-OPEN6 (f28 pointer b400469f; verify unres=4,new=5).
  FOR THE OWNER / final report: (a) D395 changes DNS serving semantics (answers from a replaced publication
  for 1 s; a zone keeps answering across its own DNSZone write) and inverted or loosened seven older
  assertions that pinned the old behavior; (b) F28-OPEN6 loosened the never-SERVFAIL sampler to tolerate
  gaps up to 10 s (observed <= 1.1 s); (c) NEW product defect seen with and without D395: a primary child
  zone under a primary parent can leave parent or child answering SERVFAIL for good ("zone awaiting
  committed publication"; evidence/CORE-COMMIT-GAP/fix-08, fix-13, fix-15, fix-18) -> needs a package
  (CORE-CHILD-ZONE); (d) TestRunServeSQLiteLongApplyKeepsServingDNSAndReads fails ~half the time without the
  race detector on this loaded machine on its read assertion (not on hosted so far).
- 2026-11-13 08:55 UTC F28-OPEN6 BACKED OUT of the local PR branch (never pushed): with it merged,
  cmd TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart fails every run ("managed recursive runtime
  did not become ready"; passes 3/3 at 77d2c9cb without it; evidence/INTEGRATE-26/recursive-*.log), and e2e
  TestEnrolledManagedAnchors{PartitionRestartAndRevoke,PromoteAndRejoin,OfflineRecovery} fail on root
  acquisition counts (evidence/INTEGRATE-26/e2e.log). f28 pointer stays 97c9117b; commits 1ef0b201..b400469f
  are on wip/v020/f28 only. When the f28 run (F28-RFC) completes, resume it with an appended package
  F28-OPEN7: fix the regression (suspects 1ef0b201 "admit writer commands as soon as a managed writer is
  ready" and a23db4d3 "keep a due first anchor step ahead of writer services"), make the acquisition counts
  exact, fold in F28-REJOIN-COUNT's finding, and remove the 10 s SERVFAIL tolerance.
  Local head 77d2c9cb = 85913663 + CORE-COMMIT-GAP; rechecking (INTEGRATE-27) before push.
  diskguard relaxed to 120 min / 75 min (it broke builds twice at 60 min); stale /tmp fixtures removed.
- 2026-11-13 09:20 UTC pushed d063f929: CORE-COMMIT-GAP (D395) + DASH-CLUSTER (dash pointer 280d0087; verify
  unres=0,new=2: the invitation view cannot be closed except with Done after a slow response -> DASH-OPEN).
  Local dashboard vitest is unreliable at load 20 (10 timeouts/lookup failures in INTEGRATE-28); trust the
  hosted dashboard job. Hosted ci on 85913663 failed: "rejoin acquisitions 5", static twin login 503, and a
  test-only DATA RACE in TestAnchorEdgeActivationJoinedFollower (joinPrimaryOn writes
  p.opts.Config.Node.DataDir at cluster_join_test.go:51 on a running runtime while Prepare marshals the
  config) -> add to F28-OPEN7.
- 2026-11-13 09:35 UTC CI-RED-2 verified unres=2,new=8. Merged locally only its clean test-only commits:
  f90fadbe, faad35d4, 976ff9c5, e2280a59, 5318385e, 271e77bd. HELD on wip/v020/ha (not in the PR):
  5edb45b2 (login reorder; conflicts with serve_anchor_activation_test.go; obsolete once D399 lands),
  4b78eec5 (fix(dns): SERVFAIL when an UPDATE's keyring stops being current; its new test
  TestRootMemberFollowerUpdateAcrossInstallAnswersServfail failed 4/4 pinned in verify), 1621b669
  (fix(anchors): capture again; conflicts with F28 85d1e142), 93a9a09b (pins the login 503 that D399
  removes). ha pointer stays 923d7720 with those six cherry-picked out of order: for the next ha integrate
  use `git cherry -v` / pick by hand. After HA-OPEN5: package HA-OPEN6 = rebase the held product fixes on
  the PR head, drop what D399 obsoletes, fix the flaky new tests and the verify notes (unauthorized peer
  pull after a commit, updater stop logged as terminal on shutdown, unbounded capture retry).
- 2026-11-13 09:45 UTC DASH-OPS merged locally (verify 0/0; dash pointer 5c4b0852; dash worktree reset to
  bdb16c65). New dash run wf_c3c5f4e1-b16 (task wlvudyj7o): DASH-OPEN, DASH-RPZ. Still to schedule for the
  dashboard when their APIs are merged: Integrations page (INT-ADMIN-API), inventory view (ID-INVENTORY),
  IAM writer forms (IAM-WRITERS). The AccessPage audit test is slow at base too (15 s alone), not a
  regression of the follower change.
- 2026-11-13 10:10 UTC merged locally on top of d063f929 (unpushed): CI-RED-2 clean test commits, DASH-OPS,
  INT-ADMIN-API (D400; int pointer ad33a576; verify clean) + my dashboard follower-table fix 2a15e5a0,
  G21-REFUSAL-INVENTORY (flake pointer 3cd42e8f; verify unres=2,new=4). flake worktree reset to 01cf27b1; new
  flake run wf_b77fe81b-98c (task w3gs5arqx): CORE-CHILD-ZONE (D404), G21-REFUSAL-OPEN. Next free: D405.
  Lesson: after any merge that adds or reclassifies a mutation route, run dashboard
  `bunx vitest run src/lib/follower.test.ts` (the dashboard keeps its own table) as well as the Go counts.
- 2026-11-13 10:35 UTC G21-REFUSAL-INVENTORY BACKED OUT of the local PR branch (never pushed): on the merged
  tree its tests fail by design for every operation other lanes added without an inventory line or hint
  (listRPZFeeds, getBlockListRPZFeeds, getClusterAnchors, edge blocks, integration manifests and writes,
  hintless 503s in authz.go, edge_blocks.go, integration_objects.go, integrations.go, object_command_write.go;
  evidence/INTEGRATE-32/libs.log, cmd.log). The flake lane worktree base 01cf27b1 = local head WITH the
  inventory, so package G21-REFUSAL-OPEN (second in that run) sees the failures and must fix them; when it
  is verified, integrate with `integrate.sh flake 2a15e5a0 <end>` (picks the five inventory commits too).
  flake pointer for the PR = 2a15e5a0 (not a lane commit; special case).
  Local PR head 2a15e5a0 = d063f929 + CI-RED-2 clean test commits + DASH-OPS + INT-ADMIN-API + follower
  table fix; pre-push INTEGRATE-31 passed on it.
- 2026-11-13 11:05 UTC pushed daff96e5 (CI-RED-2 clean test commits, DASH-OPS, INT-ADMIN-API + follower table
  fix, F28-REJOIN-COUNT [obs pointer bd10f655], RPZ-OPEN [qual 17b7c87a]). Hosted ci on d063f929 failed:
  "rejoin acquisitions 5" (fixed in daff96e5), cmd TestIntegrationWebhookMountedClientsRestart ("complete
  assembly did not converge", 20 s) and internal/rpz TestBINDRPZRuntimeTransfersRefreshRestart/tcp: both
  first sightings, right after D395 landed -> watch the run on daff96e5; if either recurs, package it.
  Merged locally: F28-RFC (D398; verify 0/2) and LEDGER-SPLICE-2 (qual pointer 58325bbf; verify 0/1).
  Lanes f28, qual, obs reset to 7b11a139. New runs: f28 wf_d66f2a1a-160 (task w6pf5x25s) F28-OPEN7 (re-applies
  the held commits from branch wip/v020/f28-held); qual wf_35967666-db3 (task ws2h86b93) G24-LOCAL, G25-DOCS.
  obs lane idle (free worktree at 7b11a139). To schedule: dashboard Integrations page (dash lane after its
  run), HA-OPEN6 (after HA-OPEN5), third ledger splice + G25-PRBODY + final G22-G25 run at the end.
- 2026-11-13 11:25 UTC DASH-OPEN merged locally (dash pointer 65660965; verify 0/0) + my fix faa6c4e1
  (FORWARDED_ROUTES needs every forwarded op: second dashboard table to update when routes change).
  Local head faa6c4e1 = daff96e5 + F28-RFC + LEDGER-SPLICE-2 + DASH-OPEN; push when ci on daff96e5 reports.
- 2026-11-13 11:40 UTC DISK HIT 100% at ~11:25 (guard at 120 min was too lax; cache grew 24 G in 2 h).
  Recovered by trimming >40 min; new guard: <8 G -> trim >75 min and stale /tmp, <5 G -> trim >40 min, 60 s
  loop. Repo and worktrees intact; lanes kept running.
  CORE-OPEN4 merged locally (core pointer 57ba9c26; verify unres=3,new=3). It contains its own fix for the
  child-zone defect (c3c4be32, 87618b4a, child back in the commit-gap test) while the flake lane's
  CORE-CHILD-ZONE has committed a different fix for the same defect (6ee32906, base 01cf27b1 without
  OPEN4). When CORE-CHILD-ZONE verifies: do NOT blind-merge; run a reconcile package (keep one mechanism,
  both tests). Also from CORE-OPEN4 verify: a POST answered 503 `current writable serving generation
  unavailable` can still have taken effect (test accepted a later 409 already-exists) -> must be settled in
  CMD-RECAPTURE's follow-up: either that 503 is truly effect-free or it must be `outcome unknown`.
- 2026-11-13 12:20 UTC pushed 72c887ef (F28-RFC, LEDGER-SPLICE-2, DASH-OPEN + route table fix, CORE-OPEN4 +
  my lint fix: listAllowed added to permissionCheckCallees) and then the e2e timeout commit (HEAD).
  FINDING: hosted e2e on daff96e5 had NO failing test; `go test ./test/e2e` hit -timeout 20m (the
  "FAIL test/e2e 1320 s" seen in every recent run is this kill). E2E_FLAGS now 45m, job 75 min. So on
  daff96e5 the only real hosted failure was internal/cluster/replica
  TestWriteWhoseDialRunsIntoTheDeadlineStaysUnknown (timing: got `primary unavailable ... i/o timeout`,
  HA lane test from D381) -> HA-OPEN6 list. Login 503 did not show on that run (intermittent).
- 2026-11-13 12:50 UTC CMD-RECAPTURE (D399) merged locally (ci pointer 10587ce2; verify unres=3,new=4) + my
  commit making its mounted proof no-race-only (t.Skip under race + NORACE_TESTS). D399 removed the
  superseded-before-dispatch refusal but NOT the hosted family: the commoner refusal is 503 generation_pending
  at capture because publication starves (Publisher.Run restarts preparation on every generation/Wake).
  New package PUBLISH-STARVATION (D405) on the ci lane (reset to 154a779a): wf_9b76b2ca-bdc, task wimisji0o.
  Next free decision: D406.
- 2026-11-13 13:15 UTC merged locally: DASH-RPZ (dash pointer 88089934; verify 0/1), G24-LOCAL (qual pointer
  7acc069b; verify unres=2,new=3: the goreleaser snapshot has NOT run for lack of disk [needs ~14 GB free];
  the live recursion check found and fixed a real boot defect 56443b9a) + my script fix. After D399 one
  zone-group test (TestServingObjectCommandResolvesZoneGroupInTransaction) failed 1 of 6 with the residual
  503 -> PUBLISH-STARVATION owns it. dash worktree reset to 00cd1b13; new dash run wf_1d0d71e5-d26 (task
  w71cuxv6g): DASH-INTEGRATIONS. Local head = a3b5a1d0 + D399 + DASH-RPZ + G24-LOCAL; push when ci on
  a3b5a1d0 reports.
- 2026-11-13 13:40 UTC pushed c5b0637c: D399 (CMD-RECAPTURE), DASH-RPZ, G24-LOCAL + script and lint fixes.
  Hosted ci on a3b5a1d0 (first run with the 45 min e2e limit; logs evidence/INTEGRATE-36/ci-a3b5a1d0-*):
  test/e2e PASSED; failed: test/interop TestBINDManagedRootRevocation ("committed refresh: 1 managed
  acquisitions, want 3" / "want 2"; modified by F28-RFC 43bbfad4 -> F28 lane), dashboard-e2e zones.spec.ts
  (PUT /objects/dnszones 503 current writable..., took 11 ms) and cmd
  TestRunServeSQLiteZonesAnswerAcrossCommits (POST /objects/blocklists 503, took 274 ms): both the 503
  family, pre-D399 head. Watch the run on c5b0637c for what D399 changes.
- 2026-11-13 14:00 UTC G25-DOCS merged locally (qual pointer bf156c6b; verify 0/2). qual lane idle (worktree
  at 223269c8); obs lane idle. HA-OPEN5 verified unres=5,new=3 but is NOT merged: it sits on the held
  CI-RED-2 commits. Saved branch wip/v020/ha-held (923d7720..7ac4f5a2); ha worktree reset to 223269c8; new
  ha run wf_07eabec6-e77 (task w5z53sq5t): HA-OPEN6 re-applies and repairs them (D381). OWNER DECISION
  pending from HA-OPEN5: a large multi-document apply through a follower is still cut at the follower's
  next pull (D375): accept for v0.2.0 or require the fix (HA-OPEN6 item 6 measures it at head).
  G24 snapshot (goreleaser --snapshot) still not run: needs ~14 GB free disk; do it in the final pass with
  lanes stopped (hack/release-local-checks.sh --full-snapshot).
- 2026-11-13 14:40 UTC merged locally on c5b0637c: G25-DOCS, IAM-WRITERS (D396; core pointer 0d969081; verify
  unres=2,new=3: permanent security floors grow without bound toward the 4096-row cap = deletes and sign-ins
  fail after ~4000 delete cycles -> needs a decision/fix; proofs partly missing), INT-UNIFI-PUSH (D401; int
  pointer bed36205; verify unres=1,new=5) + my contract commit (6 reservations removed from
  permissions.yaml, dashboard follower table, mounted follower count 58/48, api counts 34/14/58).
  prepush.sh fixed for batches without Go files (gofmt on an empty list read stdin and hung 30 min).
  CHECKLIST after merging any package that adds routes: (1) internal/api follower_test counts, (2) cmd
  serving_follower_test counts, (3) permissions.yaml reservations, (4) permissionCheckCallees for new
  helpers, (5) dashboard src/lib/follower.ts PERMISSION_OPERATIONS + DOCUMENT_FORWARDED + FORWARDED_ROUTES,
  (6) later the refusal inventory lines (flake lane, not merged yet).
- 2026-11-13 15:10 UTC D399 (CMD-RECAPTURE) REVERTED on the PR branch (two revert commits + doc fix 95c57113).
  Hosted ci on c5b0637c and a local rerun failed seven existing command tests deterministically with it:
  TestServingAdoptionActualHTTPOriginalWriterProofAndLease/stale-lease (412 want 503),
  TestServingCacheCommandRefusesOriginalAuthorityLoss/keys, TestServingDomainCommandRetainsOriginalLeaseAndPlan/stale,
  TestServingNodeCommandActualRefusesStaleAuthorityAndFinalExpiry/rotated-keys,
  TestRunServeSQLiteControlSettingsRestart ("superseded control request: status=200"),
  TestServingSourceCommandRetainsOriginalRequest, and its own
  TestServingCommandRecaptureSignInSupersededBeforeDispatch (audit rows 1 want 2). Logs:
  evidence/INTEGRATE-38/ci-c5b0637c-*.log, evidence/INTEGRATE-40/d399-regress-local.log. All pass after the
  revert (evidence/INTEGRATE-41/after-revert.log). MY MISS: prepush's cmd step only runs new tests; a
  change to a shared path needs the full cmd suite (hack/go-test.py) before push.
  The ci lane (PUBLISH-STARVATION, running) is based on 154a779a WITH D399. When it completes: do not merge
  as is; next ci package must make D399 + D405 pass the FULL cmd/kudurru suite (those seven tests state the
  contract: a rotated key, a stale lease, lost authority or a superseded control request is refused, never
  recaptured) and re-apply onto the PR head. ci pointer for the PR: nothing of D399 is in.
  Hosted on c5b0637c also: test/interop TestBINDManagedRootRevocation failed again (2nd run in a row;
  new test of F28-RFC) -> F28 lane. Local once: TestIntegrationUniFiPushMirrorPartialCreate (/sync 503
  integration_uncertain), passed on rerun -> INT follow-up.
- 2026-11-13 15:45 UTC pushed (HEAD after revert): G25-DOCS, IAM-WRITERS, INT-UNIFI-PUSH, contract commit,
  D399 revert. Local flakes seen once each under load, pass on rerun: ee/integration/host
  TestActualSQLiteSourceCommittedPendingSurvivesAckDeadline ("ack timeout lost durable outcome"),
  cmd TestIntegrationUniFiPushMirrorPartialCreate.
- 2026-11-13 16:05 UTC CORE-CHILD-ZONE (flake; D404) verified unres=1,new=4 but NOT merged: it overlaps the
  child-zone fix already merged with CORE-OPEN4 (same files in internal/primaryruntime). Its commits are
  named wip/v020/flake-childzone (01cf27b1..34ca179e). Package CORE-CHILD-RECONCILE on the qual lane
  (reset to 95c57113): wf_52bc4eea-c2d, task wt4rr63iv; it also runs the full cmd suite. When it verifies,
  merge it and treat the flake lane's CHILD-ZONE commits as superseded (when integrating flake later,
  skip 6ee32906..34ca179e and pick only the refusal inventory + G21-REFUSAL-OPEN commits).
  A full local cmd suite run on 95c57113 is in the background (task bur0fdiwt,
  evidence/INTEGRATE-41/full-cmd.log).
- 2026-11-13 16:50 UTC hosted ci 43447535882 on 95c57113 (D399 reverted): test/e2e PASS. Failing:
  cmd TestRunServeSQLiteZonesAnswerAcrossCommits + TestRunServeStaticTwinsReadOnlyPeerLossRestart (503
  current writable... = D399/D405 rework), cmd TestIntegrationWebhookMountedClientsRestart ("complete
  assembly did not converge", 2nd hosted sighting) and
  TestIntegrationDeletePurgeFencesAChangedObjectAndFinalizeRefusesLeftovers (first sighting) -> INT
  follow-up package; test/interop TestBINDManagedRootRevocation (3rd run in a row, deterministic on hosted:
  "committed refresh: 1 managed acquisitions, want 2/3") -> F28 follow-up package (append to the f28 run
  when F28-OPEN7 completes). Logs evidence/INTEGRATE-41/ci-95c57113-*.log.
  Local full cmd run (loaded machine, evidence/INTEGRATE-41/full-cmd.log) so far: batch 3
  TestRootMemberForwardPasswordAndTokensActualHTTP (503 generation_pending), batch 4
  TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages, TestCLIOIDCBrowserActualTLSProofRefusals.
  INT-EXTERNAL-ZONES verified (unres=2,new=3), to merge after the full run ends (int bed36205..).
- 2026-11-13 17:45 UTC pushed 2e15060e: INT-EXTERNAL-ZONES (D402; int pointer 841c5e99; verify unres=2,new=3).
  Merged locally: DASH-INTEGRATIONS (dash pointer 7afa8e39; verify 0/1). dash worktree reset to 229b5466; new
  dash run wf_7cdc581e-adc (task wslu6450z): DASH-IAM. Still to schedule for dash: inventory view after
  ID-INVENTORY merges.
  INT follow-up list (append a package to the int run when INT-GROUP-VIEWS completes): integration host keeps
  reporting the revision it captured after its own status write, so GET /integrations/{ns}/{name} answers
  statusKnown false for a healthy instance and a reviewed command can get 409 (ee/integration/host
  lifecycle.go; found by DASH-INTEGRATIONS); hosted-intermittent tests
  TestIntegrationWebhookMountedClientsRestart ("complete assembly did not converge", twice) and
  TestIntegrationDeletePurgeFencesAChangedObjectAndFinalizeRefusesLeftovers; local-intermittent
  TestIntegrationUniFiPushMirrorPartialCreate, TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages,
  ee/integration/host TestActualSQLiteSourceCommittedPendingSurvivesAckDeadline; the open verify items of
  INT-ADMIN-API, INT-UNIFI-PUSH and INT-EXTERNAL-ZONES in followups/wave3-open.json.
- 2026-11-13 18:40 UTC DASH-INTEGRATIONS + helper commit 649c1403 (mock manifests = Go schemas after UniFi
  push; obs pointer 649c1403) merged locally. Add to the INT follow-up list (server, found by the helper):
  kubeRefusalAdmitsCleanup (cmd/kudurru/integration_kube.go:232) admits cleanup only for
  RecordPublicationUnsupportedOnKubernetes, so a Delete-policy UniFi instance that gains a push switch
  stays Terminating; the Kubernetes refused list names all of push.mirrorRecords (refused only when
  enabled) and omits dnsServers and networks; the manifest gives no per-field refusal reason; stale doc
  comment internal/api/integrations.go:75 ("for example spec.unifi.push"); schema format ipv4 is looser
  than validatePush.
- 2026-11-13 19:20 UTC merged locally on 2e15060e: DASH-INTEGRATIONS + mock fix, ID-INVENTORY (D397; core
  pointer fabb3da0; verify 0/3; NOTE its verifier saw TestRunServeUniFiHostnamePolicyLifecycle fail 4/4
  under race at the lane head: check on the merged tree), and five D405 commits from the ci lane that do
  not need D399 (5b200f28 chunk sweep, f66940d5 Wake coalescing, a141541e, 5e8dd096, 53eea181).
  PUBLISH-STARVATION verified unres=3,new=2; the rest of it and D399 are on wip/v020/ci-held
  (e4e0a059..f1279497). ci worktree reset to 606a53a8; new ci run wf_9320b9ea-2ca (task w8n4xwyg5):
  D399-REDO with the full cmd suite as acceptance. core lane finished and idle (worktree at 606a53a8).
  Full local cmd suite on 606a53a8 running before push (evidence/INTEGRATE-44/full-cmd.log).
- 2026-11-13 19:50 UTC hosted ci on 2e15060e: only three failing tests: cmd
  TestRunServeDirectAnchorsFencedAndConflictRestart + TestRunServeSQLiteZonesAnswerAcrossCommits (503
  family -> D399-REDO) and test/interop TestBINDManagedRootRevocation (-> F28-REVOKE-HOSTED). test/e2e,
  fuzz, dashboard, dashboard-e2e, helm, lint, postgres, go test 2/2 all green.
  F28-OPEN7 verified unres=3,new=4 (f28 pointer for the PR still 13d5bec3; commits 7b11a139..b22b24c1+ on
  the lane; merge after the local full cmd run). It fixed the SERVFAIL at authority transitions
  (a09860c6: writeBlocked held across the fence fsync) and showed the INTEGRATE-26 count failure came from
  MY merge dropping F28-REJOIN-COUNT's blocking-switch wait, and that the recursive readiness failure is
  load (same at base). Still open there: fresh managed recursive node takes ~9 s to ready under race (10 s
  test bound). f28 run resumed with appended package F28-REVOKE-HOSTED (D398), task wdd1rlp4k.
- 2026-11-13 20:20 UTC flake lane finished (G21-REFUSAL-OPEN verify unres=1,new=3; its ee5415ca breaks two
  cmd tests). The refusal inventory (5 + 9 commits) is NOT in the PR; saved as wip/v020/flake-refusal;
  flake worktree reset to 606a53a8; new flake run wf_a12171a1-4d2 (task w7oc8ljrz): G21-REFUSAL-MERGE
  (re-apply on head, add lines for every new operation, full cmd suite as acceptance).
  dash lane finished: DASH-IAM verify 0/2 (dash pointer for the PR 7afa8e39; commits 229b5466..9ab3ae8a to
  merge after the local full cmd run). Next for dash: inventory view (API merged locally).
  Three packages now run the full cmd suite as acceptance (D399-REDO, G21-REFUSAL-MERGE,
  CORE-CHILD-RECONCILE) plus mine: expect heavy-slot contention.
- 2026-11-13 21:00 UTC ha lane finished: HA-OPEN6 (base 223269c8, which had D399; commits
  223269c8..<ha HEAD>). MERGE QUEUE after the local full cmd run on 606a53a8 (task bbz7jrn1p,
  evidence/INTEGRATE-44/full-cmd.log): (1) F28-OPEN7 (f28 7b11a139..b22b24c1 + its fix commits up to the
  commit before F28-REVOKE-HOSTED starts), (2) DASH-IAM (dash 229b5466..9ab3ae8a+), (3) HA-OPEN6. Then
  full cmd suite again before push. OWNER DECISION: D375 stands (a large apply through a follower is cut:
  40 documents applied completely 5 of 18 times, 160 never); documented in docs/deploy/sqlite-join.md.
- 2026-11-13 21:40 UTC int lane tranche finished: INT-GROUP-VIEWS (D403) verify 0/2 (int 841c5e99..55f56160 to
  merge). New int run wf_81a9b453-b88 (task wi765h0nr): INT-OPEN2 (continues on the lane head).
  MERGE QUEUE after the local full cmd run (batch 3/9 at 21:40; so far only
  TestServingMigrationCutWaitDoesNotBoundTheStepCommand failed, a deadline under load: rerun alone):
  F28-OPEN7, DASH-IAM, HA-OPEN6 (verify unres=1 blocker: a single signed UPDATE through a follower can
  still answer SERVFAIL; new=3), INT-GROUP-VIEWS. Idle lanes: core, obs, ha, dash (worktrees stale).
  Lint fix for the inventory test committed locally (3bc4c24b).
- 2026-11-13 22:40 UTC merged locally on 2e15060e (not pushed; checks INTEGRATE-45 running): everything from
  19:20 plus F28-OPEN7 + F28-REVOKE-HOSTED (f28 pointer 10baf336; REVOKE verify unres=1,new=1: the hosted
  TestBINDManagedRootRevocation failure was MY partial merge: F28-RFC's test needs the licensetest seam
  file from F28-OPEN7's 43d42037), DASH-IAM (dash pointer 90762166), INT-GROUP-VIEWS (D403; int pointer
  55f56160), HA-OPEN6 (ha pointer 4a77f00a; skipped 5d28289e duplicate capture test; dropped the D399 doc
  section in access.md and D399 field context; verify unres=1 blocker: a single signed UPDATE through a
  follower can still answer SERVFAIL). Baseline full cmd run on 606a53a8 stopped after 5 of 9 batches:
  failures TestServingMigrationCutWaitDoesNotBoundTheStepCommand (deadline) and
  TestServingTSIGFinalCommandLifetimeRollback, both to rerun alone.
  Lanes idle after this merge: core, obs, ha, dash, f28 (reset before reuse).
- 2026-11-13 22:50 UTC dash worktree reset to 531bffd0; new dash run wf_4d4cfc5a-44b (task w3hixlpb4): DASH-INVENTORY (inventory view + record views in the editor).
- 2026-11-14 00:10 UTC pushed 531bffd0 (F28-OPEN7 + F28-REVOKE-HOSTED, DASH-INTEGRATIONS + mock fix, ID-INVENTORY,
  five D405 commits, DASH-IAM, INT-GROUP-VIEWS, HA-OPEN6). KNOWN REGRESSION pushed with it:
  TestRunServeUniFiHostnamePolicyLifecycle fails under race since ID-INVENTORY (bisect: 3/3 pass at
  2e15060e, 3/3 fail at e0bddb72 right after the inventory commits; "original hostname policy unavailable
  at held request capture ... assignment_remaining=-1.5s"; evidence/INTEGRATE-45/unifi-hostname-at-*.log).
  Fix package INVENTORY-REGRESS on the core lane (reset to 531bffd0).
  Load flakes seen in INTEGRATE-45 (pass alone): internal/store/sqlite
  TestAnchorPublicationHeldAndFinalOriginalExpiryRefuseWithoutRenewal/inside-final-callback (250 ms timer),
  cmd TestRunServeKubeClientGroupViews (update conflict "object has been modified": the new test must
  retry its own write on a conflict -> INT follow-up).
  CORE-CHILD-RECONCILE verified? (qual lane finished; merge next: qual 95c57113..3e822b60+).
  INT-OPEN2 finished (int 55f56160..77fb35a3+; verify state to read).
- 2026-11-14 00:50 UTC merged locally on 531bffd0 (not pushed): CORE-CHILD-RECONCILE (D404; qual pointer
  1eb9407e; verify unres=1 minor: a new child zone answers SERVFAIL until its first publication, 2.5-4.9 s
  with 23 zones) and INT-OPEN2 (int pointer 0c48fb39; verify unres=1,new=4). INT-OPEN2 changed the manifest
  contract (fieldLimits): dashboard tsc fails until the helper agent (obs worktree at 3666e679) updates the
  mocks and the page; cherry-pick its commit before pushing. core lane: wf_22ee5141-343 (task w6y1plw8r)
  INVENTORY-REGRESS. Checks INTEGRATE-46 running (task blkru0qj0).
  When flake's G21-REFUSAL-MERGE lands: skip the flake-childzone commits (superseded by the reconcile).
- 2026-11-14 02:10 UTC hosted ci 43514296652 on 531bffd0: TestBINDManagedRootRevocation now PASSES. New: all
  four enrolled e2e tests fail ("document N of 40 after the cut at M was not refused as documented: ...
  the enrolled primary is unavailable; the write was not applied"), plus cmd
  TestRootMemberPasswordCommandActualHTTP, TestServingHTTPLocalUsersResetCASOwnershipAndDisabled (503 want
  201), TestRunServeTracingThroughCacheAndRace, anchors login 503 (logs evidence/INTEGRATE-45/ci-531bffd0-*).
  The e2e failure reproduces locally (evidence/INTEGRATE-46/e2e-partition-local.log). I reverted HA-OPEN6
  locally (88bed726) on suspicion, and the test STILL fails after it (e2e-partition-after-revert.log): so
  the cause is elsewhere in 2e15060e..531bffd0 (ID-INVENTORY, D405 chunk sweep / Wake coalescing, F28-OPEN7,
  DASH-IAM, INT-GROUP-VIEWS) or in addition. Bisect running in the obs worktree (task below; logs
  evidence/INTEGRATE-46/e2e-bisect-<sha>.log for 2e15060e, e0bddb72, 606a53a8, 9e486185, df55fcd5).
  DECIDE after the bisect: revert the culprit, and re-apply HA-OPEN6 (git revert 88bed726) if it is innocent.
  NOTHING pushed since 531bffd0; local = 531bffd0 + CORE-CHILD-RECONCILE + INT-OPEN2 + dashboard manifest
  commit + HA revert. Local selection also failed TestIntegrationUniFiPushMirrorRecordsReviewed (/sync 503
  integration_uncertain) and TestRunServeWebhookRecordsFileIntegrationAndCLI ("CLI check: unavailable:
  original integration operation unavailable") under load.
  dash lane finished DASH-INVENTORY (verify unres=1,new=3; dash 531bffd0..ffe17c08+ to merge).
- 2026-11-14 03:20 UTC BISECT RESULT: the enrolled e2e failure ("the enrolled primary is unavailable; the write
  was not applied" during a 40-document apply through a follower) comes from the five D405 commits I
  cherry-picked early from PUBLISH-STARVATION (zone chunk sweep 5b200f28, Wake coalescing f66940d5, a141541e,
  5e8dd096, 53eea181): e2e passes at 2e15060e and e0bddb72, fails at 606a53a8
  (evidence/INTEGRATE-46/e2e-bisect-*.log). Reverted them (bae1c9df) and restored HA-OPEN6 (832dab99 undoes my
  mistaken revert 88bed726). With that, TestEnrolledClusterProcessesPartitionCatchupAndRestart and
  ...PromoteAndRejoin pass locally (e2e-after-d405-revert.log).
  CONSEQUENCE for the ci lane: D399-REDO is based on 606a53a8 WITH those commits. When it returns, the
  enrolled e2e tests (TestEnrolledClusterProcesses*, TestEnrolledManagedAnchors*) are part of the bar, and
  the chunk sweep / Wake coalescing must be fixed for follower forwarding (the primary answers `unavailable`
  to forwarded items) before they come back. Also re-check that the D405 revert did not strand
  DASH/other merges (build is clean).
  LESSON: run the enrolled e2e tests (at least PartitionCatchupAndRestart, ~4 min) before pushing any change
  to serving, store, zonestate or cluster code; add to the pre-push routine.
- 2026-11-14 04:10 UTC pushed bae1c9df: CORE-CHILD-RECONCILE, INT-OPEN2, dashboard manifest commit, D405 five-commit revert, HA-OPEN6 restored. Pre-push and dashboard checks green (INTEGRATE-47); two enrolled e2e tests pass locally. Next: merge DASH-INVENTORY (dash 531bffd0..), then wait for hosted ci on bae1c9df.
- 2026-11-14 05:30 UTC owner asked "Did this finish?": answered no, with status. Local stack on pushed
  bae1c9df: DASH-INVENTORY (verified head 81c2d3af; checks INTEGRATE-48 green + one enrolled e2e pass) and
  G21-REFUSAL-MERGE (flake pointer ccba4616; verify unres=4,new=3; checks INTEGRATE-49 running). Push
  81c2d3af first (`git push origin 81c2d3af:robertherbaugh/v020-features-review`) when hosted ci on bae1c9df
  reports; its go test 1/2 job has been running unusually long (>75 min).
- 2026-11-14 05:50 UTC HOSTED CI GREEN: run 43532858454 on bae1c9df completed/success (all jobs). First green ci of
  this pass. Pushed 81c2d3af (DASH-INVENTORY) on top. Local beyond it: G21-REFUSAL-MERGE (checks INTEGRATE-49).
  Known but not failing on that hosted run: TestRunServeUniFiHostnamePolicyLifecycle regression under
  local race+load (INVENTORY-REGRESS in progress); the 503-after-ready family is intermittent (D399-REDO).
- 2026-11-14 06:40 UTC pushed 7663c957: G21-REFUSAL-MERGE (flake pointer ccba4616; walks reach 212 operations on
  primary, follower, fenced and conflicted in e2e; INTEGRATE-49 all green) + two fixes for the workflows
  that failed on bae1c9df although ci was green: docs (a closing fence sharing its line with prose in
  docs/architecture/integrations.md swallowed the Kubernetes execution heading; docs-check-links passes
  locally now) and security/sast (semgrep detect-non-literal-regexp on the dashboard's manifest pattern
  check; annotated nosemgrep with the reason, as lib/lists.ts does; NOT rerun locally, hosted decides).
  After a push check ALL workflows on the head (gh run list --commit), not only ci.
  Idle lanes: flake, dash, ha, f28, int, qual, obs. Running: ci (D399-REDO fix), core (INVENTORY-REGRESS
  review).
- 2026-11-14 06:55 UTC lanes qual, dash, ha reset to 7663c957. New runs: qual wf_b43e1557-250 (task wszr105mv)
  LEDGER-SPLICE-3; dash wf_475b96be-132 (task wk4ttszw7) DASH-OPEN2; ha wf_0fc213f8-4a7 (task w43iixdus)
  HA-OPEN7 (D381/D375). Still running: ci wf_9320b9ea-2ca D399-REDO (fix), core wf_22ee5141-343
  INVENTORY-REGRESS (review). Idle: flake, f28, int, obs. Candidates for them: F28 fresh managed recursive
  node readiness (~9 s under race vs 10 s test bound) and updater outcome notes; INT-OPEN3 (UniFi partial
  create 30 s stall, TestRunServeKubeClientGroupViews update conflict, permanent-setup retry); the final
  pass (snapshot needs ~14 GB free: stop lanes first).
- 2026-11-14 07:25 UTC LEDGER-SPLICE-3 merged locally (qual pointer 2c588c2e; verify 0/1; docs checks pass). On 7663c957 the docs and security workflows PASS (both fixes worked); ci in progress. Push the tracker update when ci on 7663c957 reports.
