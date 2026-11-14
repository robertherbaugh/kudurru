### DOCS-OPS operator documentation for shipped v0.2 features — November 10, 2026

Map entries G21-14 and G25-OPDOCS. No decision number: no design decision was
needed. Base commit `51878320` (lane branch `wip/v020/docs`, after DOCS-ARCH).
Ten source commits: `e4a7c586` (enrolled cluster guide), `fa515d4a` (dynamic
update guide and stored TSIG keys), `fafe2b4a` (Kubernetes role bindings and
grant floors), `05d20854` (deployment index), `418066be` (runbook for a lost
primary), `ba7d9fb9` (upgrade runbook pointer to the IAM migration),
`ca2b8965` (Access screenshots and the screenshot script), `4e2f1851` (guide
text), `12b3e022` (sign-in page) and `7c7c195b` (note about the Actions
column). A second review round added six: `4cd4505e` (every update policy
that stops a SQLite zone), `055630d0` (update answers for a key without a
grant), `d4af05c0` (CLI password login on a follower), `350efba0` (what a
follower passes on and how apply behaves), `4f4c27fb` (order of the v0.1
upgrade around the CRD step) and `5f10ed88` (the eleven errors of the guide
checker). No generated commit: the package changes no generator input, and
`make docs-gen` leaves the generated reference unchanged.

Operators now have pages for the v0.2 features that were only described in
the architecture tour or the CLI reference, and the stale lines named in
G21-14 are corrected. The procedures of the new SQLite pages were run against
real `kudurru serve` processes on loopback with a binary built from the lane
head. The first version of two pages did not state everything those runs
answered: the second review found three wrong or incomplete statements and
two behaviors that the first runs had recorded and the pages and this ledger
had left out. The section "Review fixes" below lists them, and after it the
pages state what the runs answer, with one stated dependency on the HA lane.
The Kubernetes statements were checked against the code and the existing
envtest tests, because no cluster is available here.

What the pages now say:

- **Enrolled SQLite cluster (`docs/deploy/sqlite-cluster.md`, new).** The
  whole life of a cluster: what to prepare (the same explicitly configured
  encryption key on every member, member names and internal addresses, a
  token-based CLI context for each member), activation of the first primary
  (`cluster enrollment`, `cluster activate`, restart), invitation and offline
  join, checking the members, what a follower passes on to the primary and
  what its two 503 answers mean, how `kudurru apply` and `kudurru restore`
  behave with several documents, promotion (`cluster promotion`, `cluster
  promote`, the meaning of each `reason`), the return of the old primary,
  recovery after two promotions, the limits of this version and a table of
  the answers of the cluster commands, with a small state diagram. The join
  page, `compose-ha.md`, `deploy/compose/ha/README.md`, the deployment index
  and the site's "Choose a deployment" table link to it. The join page keeps
  its sections: the HA and F28 lanes are editing it, so only a three-line
  pointer was added at its top. `docs/deploy/runbooks.md` gains "The primary
  of an enrolled SQLite cluster is lost", and its upgrade section now points
  a Kubernetes installation that comes from v0.1 to the offline identity
  migration of `iam-upgrade.md`, which nothing linked from there.
- **RFC 2136 (`docs/deploy/dynamic-updates.md`, new).** Key creation (file key
  or stored key through `kudurru api`), `spec.policy.update` and
  `spec.policy.updatePolicies`, the four things to check in a policy because
  the API accepts policies that take a SQLite zone out of service, an
  `nsupdate` example, what an update may not change, a table of answers (with
  `SERVFAIL` on SQLite for a key that no grant of the zone names), SQLite storage and backup of dynamic records,
  the enrolled cluster (updates go to the primary; a follower answers
  `SERVFAIL`), static twins, and on Kubernetes the `rfc2136-<zone>` record set
  (owner `dns-update`, namespace rule, eight attempts within ten seconds, do
  not hand-edit, selector labels, backup and restore).
- **Kubernetes role bindings (`docs/deploy/rolebindings.md`).** "Kubernetes
  durable accepted-authority work remains open" is replaced by a section on
  what Kubernetes does (D364): frozen roles of sessions and personal tokens,
  removal and regrant, the `kudurru-grant-floors` Secret, what to avoid, the
  fail-closed start, the unobserved removal while no replica runs, the limits
  and Helm's handling. `helm.md` and `kubernetes-service-account-auth.md`
  point to it.
- **Stored TSIG keys (`docs/deploy/tsig-keys.md`).** "once at startup" and
  "Full supported-driver rotation qualification remains separate" are gone;
  the page shows the `kudurru api` calls and where a zone names a key.
- **Administrator guide.** Chapter 9 no longer mentions "Read-only in this
  release" and gains "Use an authenticator" (set up, replace, remove, remove
  for another person) and "See and end your browser sessions"; chapter 10
  gains "Sign in with single sign-on" (browser and `--device`) and a table of
  the `cluster`, `iam`, `integrations`, `anchors` and `api` command groups;
  chapter 11 gains "Recover a lost authenticator" (host token, `kudurru api`);
  chapter 7 points to the dynamic update page. The dashboard pages for Access
  and Identity follow, and the sign-in steps of chapter 2 and of the first
  steps mention the Authenticator code field. The screenshots of the Access
  page (three) and of the sign-in page are retaken with the repository's
  script. `hack/guide-lint.py` reports no error: the new text had none, and
  `5f10ed88` rewords the eleven older places that it reported in chapters 7
  and 8 and on the Upstreams page (five `may`, one `etc.`, four long
  sentences, one long paragraph) without changing what they say.
- **Index (`docs/deploy/README.md`).** A row for the enrolled cluster and a
  task table for twelve more pages the index did not link.

Evidence under `.context/v020-final-20261110/evidence/DOCS-OPS/`. The runs use
`docsrun.py` (in that directory), which drives the built CLI, `dig`, `nsupdate`
and the HTTP API against real processes and redacts secrets:

- `build02.log`: `CGO_ENABLED=0 go build ./cmd/kudurru`, plain and with
  `-tags=licensetest`, both exit 0 (26 s under the lock). `build01.log` is a
  build that was canceled before it got the lock. Both binaries were removed
  after the runs.
- `run-cluster03.log` (license-test binary and the repository's test key,
  2 min 5 s): 46 checks pass, none fail. Three members: activation (committed,
  restart required, `member-identity.json` written, a repeat with the old
  review answers conflict), invitation (a second one with the old review and
  one with an expiry of 15 minutes answer conflict), offline join (refused
  without the encryption key before any change; `primary.committed` and
  `installed`), a token made on the primary accepted by the follower after
  5.0 s, a member started without the shared key ready but not receiving new
  data, promotion eligible 13.6 s after the primary stopped (`missedPulls: 3`,
  10.3 s unreachable), a wrong `--confirm-node` and a reused review refused,
  epoch 2, the third member following the new primary, and the old primary
  showing `mode: follower` and the new data after its restart.
  `run-cluster04-no-license.log`: the same 46 checks pass with the plain
  binary and no license key (see the open points).
- Retained failed runs: `run-cluster01.log` (25 pass, 13 fail) and
  `run-cluster02.log` (38 pass, 5 fail). The checks failed on assumptions of
  the run script, and each failure showed a behavior the page now states or
  the open points list: `kudurru apply` of a zone and
  its record set in one file answers 503 for the second document on a fresh
  node; a token is rejected by a follower until its next pull; a too-distant
  invitation expiry answers 409 conflict, not 400; the API of a just-promoted
  member answers `503 generation_pending` for about a second (0.7 s measured).
- `run-updates02.log` (plain binary, 34 s): 68 checks pass. Stored key through
  `kudurru api`, granted add over UDP and TCP, serial advance, exact-name TXT
  grant, `REFUSED` for an ungranted type, the apex, apex NS, a record set's
  name and type and a name with a record-set CNAME, `REFUSED` unsigned,
  `NOTAUTH(BADKEY)` and `NOTAUTH(BADSIG)` (both shown by nsupdate as "clocks
  are unsynchronized"), the four prerequisite answers, all-or-nothing, delete
  of a whole name, `records list`, `zones export`, absence from `kudurru
  backup`, survival of a record set change and a restart, `SERVFAIL` for a
  signed update to a zone with `update: deny`, and for `update:
  useNetworkACL` and a grant name outside the zone: zone `SERVFAIL`, node not
  ready, `READY False` (`AwaitingCommittedPrimary`), recovery after
  correction. Retained failed run `run-updates01.log` (34 pass, 9 fail): the
  same multi-document apply failure, after which the record-set checks ran
  without a record set.
- `run-totp01.log` (74 s): 23 pass. Setup (key, five-minute lifetime, SHA1,
  six digits, 30 s), confirmation signs the session out, sign-in refused
  without a code and accepted with one, CLI `--password-stdin --totp-stdin`,
  session list and revoke, `kudurru admin reset-password` keeps the
  authenticator, recovery with a host token through `kudurru api`.
- `run-follower-auth01.log`: 14 pass (authenticator setup and session revoke
  answer 503 on a follower, reads and sign-in through the follower work).
  `run-follower-update01.log`: 14 pass (a signed update sent to a follower
  answers `SERVFAIL` and is applied nowhere; the primary answers `NOERROR` and
  the follower serves the record after its pull).
  `run-invitations01.log`: 8 pass (64 invitations issued, the 65th refused with
  `503 unavailable`, also after expiry and after a restart).
  `run-static-update01.log`: a static twin with a file key accepts an update.
  `run-follower-alone01.log`: a follower restarted without its primary is
  ready, answers DNS and becomes eligible.
- `guide-lint01-baseline.log`: `python3 hack/guide-lint.py` reports 14 errors
  at the base. `final02-head.log`: 11 errors at the head of the first round;
  the three that are gone were in chapter 10 and the Access page, and none of
  the 11 is in text of this package. `fix-light01.log`: 0 errors in 67 files
  at the final head `5f10ed88`.
- `links01.log`: 19 changed or new files, 123 relative links and anchors, 0
  broken. `mermaid02.log`: the one new diagram parses with the vendored Mermaid
  bundle (`mermaid01.log` is the version with one more edge, removed because
  no test covers that transition).
- `site01.log`: Hugo build of the docs site with the Makefile's flags
  (`--panicOnWarning`, 165 pages, exit 0) and `htmltest` over the result (162
  documents, passed); the two new pages and their anchors are rendered.
- `final01.log` (under the lock, at `4e2f1851` plus the two sign-in pages, 9 s
  after a wait of 41 minutes for the lock): `make docs-gen` exit 0 with no
  change under `docs-site/content/reference` or `api`, `TestReferenceDrift`
  ok, `make version-check` PASS, `make docs-site` exit 0, `make
  docs-check-links` passed (162 documents). `final02-head.log` (final head
  `7c7c195b`, clean tree): 22 changed Markdown files with 146 relative links
  and anchors, 0 broken; `make version-check` PASS; Hugo build exit 0 and
  `htmltest` passed; the diagram parses; no change under the generated
  reference since the base. Of the five acceptance greps, `cluster activate`,
  `cluster promote`, `rfc2136-` and `kudurru-grant-floors` each appear in a
  `docs/deploy` page; `anchorMode` does not (see the open points).
- `shots01.log` to `shots03.log`: the repository's
  `dashboard/scripts/guide-screenshots.mjs` against the mock dev server with
  Chrome 156. `shots01` showed that the "token created" shot no longer
  contained the token (the two new panels pushed it below the visible area);
  the script now scrolls to the API tokens panel first, and `shots02` shows
  the token and the table. `shots03` retakes the sign-in page. The audit shot
  of the same group is not replaced. `probe-actions01.log`: at 1280 pixels the
  Actions columns of the Browser sessions and Local users tables end at 1467
  and 1607 pixels inside a 987 pixel scroll container and are reached by
  scrolling; the guide says so.
- `run-static-cluster01.log`: a static twin answers `503 unavailable` to
  `cluster enrollment` and `cluster promotion`.

Review fixes (second review round, verdict "accept with fixes"; six commits
on top of `7c7c195b`):

- **Update policies that stop a SQLite zone (`4cd4505e`).** The caution named
  two such policies. A grant name without the final dot and a `types` entry
  that is not a record type do the same. The field table now says that a grant
  name ends with a dot and what a `types` entry is, and the caution is a list
  of four checks.
- **Answers to a signed update (`055630d0`).** On SQLite a signed update from
  a key that the node has but that no grant of the zone names, also `allow`
  without a grant, is answered `SERVFAIL`, not `REFUSED`. The `REFUSED` row is
  limited to a key that a grant names, the `SERVFAIL` row has the SQLite
  case, and a paragraph says to compare the key name with the grants first.
  The cause, as read in the code (`internal/primaryruntime/update.go` and
  `sqliteadapter/adapter.go`): the planner answers `REFUSED`, the admission
  of that refusal runs an empty plan for the same key, which fails for a key
  without a current credential in the zone, and the runtime answers every
  failed admission as `SERVFAIL`. Kubernetes answers the planner's `REFUSED`
  (`cmd/kudurru/kube_update.go`).
- **Sign-in on a follower (`d4af05c0`).** `kudurru login --username` never
  works against a follower in this version: the sign-in is passed on, the
  API token that the CLI saves (`POST /auth/tokens`) is not. The browser
  sign-in works on a follower only while the primary is reachable.
- **Changes through a follower (`350efba0`).** Section 4 no longer explains a
  refusal as a race that "send it again" cures. It lists what a follower
  passes on and examples of what it does not, the meaning of `503 unavailable`
  and `503 completion_unknown`, and how a request with several documents
  behaves through a follower and on the primary.
- **Upgrade order (`4f4c27fb`).** The runbook said to migrate before the Helm
  steps, and the migration page requires the new CRDs, which are Helm step 1.
  The order is now: stop every process, step 1, migrate, step 2.
- **Guide checker (`5f10ed88`).** See above; the proof command of the
  acceptance now passes.

The review's suggestion for section 4 was to state that `kudurru apply` and
`kudurru restore` are not forwarded from a follower. That is the behavior of
this lane's base, and it is a defect that the HA lane had already fixed when
the review ran (`733eb797`, FWD-BOUND, and `29cdf81a` on `wip/v020/ha`): the
30-second request lifetime that the forward requires was installed only for
paths under `/objects/` and `/zones/`, so `POST /apply`, the `/blocklists`,
`/clientgroups` and `/policies` routes and batch deletion were refused with
"the follower's source changed". A page that said "not forwarded" would be
wrong in the release and would contradict the join page of that lane. The
page therefore describes the two lanes together, and the runs below were made
on both: the lane head, and the lane head with the HA lane's Go changes. Two
statements of section 4 hold only with the HA fix (`kudurru apply` through a
follower, and the dashboard's list editors through a follower). **Integrate
`350efba0` together with HA-FIX.** If HA-FIX were not part of the release,
those two statements would have to say "refused".

Evidence for the review fixes, in the same directory. `fix-probe.py` (and
`fix-probe-restore.py`) use the helpers of `docsrun.py`; every check is named
after the statement of the page that it stands for:

- `fix-build01.log` (under the lock, after a wait of 16 min 45 s; 18 s):
  `CGO_ENABLED=0 go build ./cmd/kudurru` twice, both exit 0. The first binary
  is the lane head. The second is the lane head with the 18 non-test Go files
  that `wip/v020/ha` (at `ba1351ac`) changed since the common base `33ee04bc`,
  supplied through `go build -overlay`; the two lanes change disjoint Go
  files (the log checks it). Nothing in the worktree was changed for it: the
  six paths that the log lists as modified are the documentation edits that
  were in progress. Both binaries and the overlay directory were removed
  after the runs.
- `fix-probe-policy02.log` (lane head, one SQLite node, two stored keys,
  1 min 57 s): 60 checks pass, none fail. Answers, four attempts one second
  apart for each case: type not granted `REFUSED`, name not granted `REFUSED`,
  key that no grant of the zone names `SERVFAIL`, `allow` without a grant
  `SERVFAIL`, `update: deny` `SERVFAIL`, unsigned `REFUSED`, a key the node
  does not have `NOTAUTH(BADKEY)`; no record stored, all zones still
  answering. A `tsigKeyRef` with the final dot and a `types` entry
  `TYPE65280` work. Six policy shapes (name without the final dot, `BOGUS`,
  `AXFR`, `OPT`, name outside the zone, `update: useNetworkACL`): the API
  accepts each one, all twelve DNS samples over six seconds are `SERVFAIL`
  with the node not ready, `zones list` shows `READY False` with the reason
  `AwaitingCommittedPrimary`, another zone of the node keeps answering, and
  the zone and readiness return after the correction. In this run the second
  of a series of identical granted updates, sent half a second after the
  first, was answered `SERVFAIL` in both series (2 of 16 updates); in
  `fix-probe-policy01.log` none of 16 was. Retained failed run
  `fix-probe-policy01.log` (55 pass, 1 fail): the probe sent `zones options
  set` with a resourceVersion read before the node wrote the zone's status
  and got `precondition_failed`, so the `OPT` case did not run; the probe now
  reads the options again.
- `fix-probe-follower-ha04.log` (lane head with the HA lane's Go changes, a
  primary and a follower, no license key, 4 min 10 s): 47 checks pass, none
  fail. `kudurru login --username` against the follower exits 1 at "mint a
  CLI token" with the primary up; the browser sign-in answers 200. Passed on:
  `records add`, `records rm`, `PUT` under `/objects/`, `kudurru delete`,
  `PUT /blocklists/...` (the route of the dashboard's editor), `blocking
  disable` and `enable`. `kudurru apply` of one document: 12 of 12 through the
  follower, 1.3 s apart. `kudurru apply` of 250 new record sets in one file:
  the first 122 created, the other 128 refused with `unavailable` (one "cluster
  authority changed", 127 "the follower's source changed"), exactly the 122 on
  the primary; twelve repeats through the follower created nothing more (each
  was cut inside the documents that were already applied); three runs on the
  primary applied 1, 0 and 1 more document and refused the rest with "current
  writable serving generation unavailable". Five new documents on the primary:
  five runs, one document each. Not passed on, each answered 503: `settings
  set`, `POST /auth/tokens`, `POST /iam/users`, `POST /tsigkeys`,
  authenticator setup. With the primary stopped: `PUT` under `/objects/`,
  `records add`, `kudurru apply` and the browser sign-in answer `503
  completion_unknown`, the token context still reads `cluster promotion`, the
  follower answers DNS, and nothing that was sent is on the primary after its
  restart.
- `fix-probe-follower-lanehead04.log` (lane head without the HA changes, 3 min
  8 s): 44 pass, 3 fail. The three are exactly the statements that need the
  HA fix: `PUT /blocklists/...` through the follower (refused, "the follower's
  source changed"), `kudurru apply` of one document through the follower (0 of
  12) and the first documents of the 250-document apply (0 of 250). Everything
  else answers as on the other binary, except that `kudurru apply` with the
  primary stopped says `unavailable` and not `completion_unknown`.
- Retained failed runs of the follower probe, each of which changed the page
  or the probe: `fix-probe-follower-ha01.log` (41 pass, 3 fail) and
  `fix-probe-follower-lanehead01.log` (39 pass, 5 fail) showed that the same
  file against the primary does not complete the apply (one document, then
  "current writable serving generation unavailable"), which the first draft
  of the section had promised, that refused documents carry two messages, and
  that the probe had chosen a setting that the node file locks.
  `fix-probe-follower-ha02.log` (45 pass, 2 fail) and
  `fix-probe-follower-lanehead02.log` (42 pass, 5 fail) showed that 40 repeats
  through the follower apply nothing more after a first request that applied
  222 of 250. `fix-probe-follower-ha03.log` (46 pass, 1 fail) and
  `fix-probe-follower-lanehead03.log` (44 pass, 3 fail) showed that a run on
  the primary can apply no document at all when it arrives while the node
  publishes the previous change, so the page says "one more document at
  most". In the four runs with the HA changes the first request through the
  follower applied 46, 222, 36 and 122 of the 250 documents.
- `fix-probe-multi-lanehead02.log` (56 s) and `fix-probe-multi-ha02.log`
  (54 s), one SQLite node that is not a cluster member: 13 checks pass on each
  binary. A file with 2 new record sets needs 2 runs of `kudurru apply`, with
  10 it needs 10 runs (15 and 12 when repeated without a pause, because a run
  that arrives while the node publishes applies nothing), with 10 new block
  lists 10 runs; a run applies one document and answers "current writable
  serving generation unavailable" for every later one, also for a later
  document that changes nothing. Retained failed runs
  `fix-probe-multi-lanehead01.log` (11 pass, 1 fail) and
  `fix-probe-multi-ha01.log` (13 pass, 1 fail): the checks expected unchanged
  documents after a changed one, and a restore of a node's own backup, to
  report `unchanged`; both are refused or changed, as stated.
- `fix-probe-restore-lanehead01.log` and `fix-probe-restore-ha01.log` (39 s
  each, 10 checks pass): `kudurru restore` of a backup with 9 documents needs
  4 runs on the node it came from (four of its documents count as changed)
  and 9 runs on a new node. In both logs the first DNS query to the new node after
  the last run timed out after two seconds and the next three were answered;
  this was not investigated.
- `fix-light01.log` (final head `5f10ed88`, clean tree): six commits with the
  trailer and subjects of 54 to 67 characters, no forbidden or generated path
  and no Go file touched, 24 changed Markdown files with 159 relative links
  and anchors and 0 broken, `hack/guide-lint.py` 0 errors, no British
  spelling in the changed files, four of the five acceptance greps found
  (`anchorMode` is not, see the open points).
- `fix-gates01.log` (under the lock, final head `5f10ed88`, 9 s after a wait
  of 5 min 52 s): `TestReferenceDrift` ok, `TestCLIReferenceDrift` ok, `make
  version-check` PASS, `python3 hack/guide-lint.py` 0 errors in 67 files,
  `make docs-site` exit 0 (165 pages), `make docs-check-links` passed (162
  documents), worktree clean afterwards.
- The reviewer's logs `review-r2-probe-*.log` show the same behaviors at the
  reviewed head and are the retained evidence of the findings.

Open, stated honestly:

- Not written here: the managed root anchor operator page and the stale
  sentence in `dnssec-validation.md` (G25-OPDOCS item 5; the F28 lane owns
  F28-RFC-4 and F28-GATE-2), `docs/enterprise.md` (item 6, outside the package
  scope), the version-pinned statements and migration comparisons (item 8,
  package DOCS-VERSION), upgrade notes from 0.1.0 beyond the pointer to the
  identity migration (item 10; the QUAL lane is deciding the upgrade
  contract), and the link from the root `README.md` (integrator-owned).
- The follower section of `sqlite-join.md` was left to the HA lane (FWD-DOCS).
  Section 4 of the new page names what a follower passes on in the terms of
  the commands and defers to that section for the routes. Its statements
  about `kudurru apply` and the dashboard's list editors through a follower
  need the HA lane's fix (`733eb797`, `29cdf81a`); see "Review fixes". Later
  HA packages that forward more (settings, identity administration) or change
  the refusals make three places of the page stale: the examples after
  "Every other change answers 503 on a follower", the answer table of
  section 4, and the sentence about `kudurru login --username` in the
  prerequisites.
- For the HA lane, from the runs of this package: the join page of that lane
  says to "run the apply again (committed documents report `unchanged`), or
  send an apply of many documents or a large restore to the primary". In
  these runs neither completes a larger file in one more step: 12 and 40
  repeats through the follower applied nothing more, and the primary applies
  one new or changed document for each request.
- Kubernetes statements (the `rfc2136-<zone>` record set, grant floors, stored
  key reload, `REFUSED` for an uncompilable policy and for a key that no
  grant names) rest on code reading and the existing envtest tests, not on a
  run in this package. The statement about `kudurru backup` and `restore` of
  the `rfc2136-<zone>` record set is from code only. On SQLite, a few answers
  that need a fault to appear were not produced and rest on the code:
  `completion_unknown`, `committed_output_unavailable`, `FORMERR`, `NOTZONE`
  and the DNAME rule. The conflict recovery commands (`cluster resolution`,
  `cluster resolve`) were not run here; the page links to the existing
  procedure, which `TestEnrolledProcessConflictResolution` covers. The CLI
  sign-in through an identity provider was not run (no provider here); its
  text follows the code and the existing dashboard page.
- Product behaviors found by the runs, documented as they are and not
  changed. The first three are for the integrator to route before the
  release; the pages can only describe them:
  - On SQLite one `POST /apply` request (`kudurru apply`, `kudurru restore`)
    applies its first new or changed document and answers `503 unavailable`,
    "current writable serving generation unavailable", for every later
    document, also for one that changes nothing. A file with N new or changed
    objects needs N runs; a backup of 9 objects needed 9 runs of `kudurru
    restore` on a new node. Seen on a node that is not a cluster member and on
    an enrolled primary, with and without the HA lane's changes. The first
    round recorded this as "the multi-document apply failure" for a zone and
    its record set and understated it.
  - With the HA lane's fix, an apply of several documents through a follower
    applies a first part (46, 222, 36 and 122 of 250 in four runs) and
    refuses the rest, and repeating the command through the follower applied
    nothing more in 12 and in 40 repeats.
  - At this lane's base, without the HA lane's fix, a follower refuses
    `POST /apply`, the `/blocklists`, `/clientgroups` and `/policies` routes
    and batch deletion every time with "the follower's source changed". The
    first round's `run-cluster03.log` (lines 193 to 199) had recorded the
    refused apply; the page and this ledger did not say so. Fixed by
    `733eb797` on `wip/v020/ha`.
  - `kudurru login --username` against a follower fails at the CLI token
    although the primary is up (`run-cluster03.log`, lines 117 to 120, also
    left out by the first round). The sign-in step of the command succeeds
    first, so the primary has enrolled a session when the command fails (from
    the order of the CLI's steps; the session list was not read).
  - Four update-policy shapes that the API accepts take a SQLite zone and the
    node's readiness down: an `update` value other than `deny` and `allow`, a
    grant name outside the zone, a grant name without the final dot, and a
    `types` entry that is not a data record type.
  - On SQLite a signed update is answered `SERVFAIL` instead of `REFUSED` for
    a zone that denies updates and for a key that no grant of the zone names.
  - On SQLite 2 of 32 granted updates of the two policy runs were answered
    `SERVFAIL`, each time the second of a series, half a second after the
    first.
  - TSIG error answers that nsupdate reports as a clock problem; `cluster
    status`, `nodes` and the license node count not seeing enrolled members,
    so the node limit is not applied to an enrolled cluster; the lifetime
    limit of 64 invitations with a misleading refusal; a member with the
    wrong encryption key reporting ready; and table action columns outside
    the visible area at a width of 1280 pixels.
- `hack/guide-lint.py` reports no error after `5f10ed88`. The linter is not a
  CI gate.
