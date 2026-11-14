### G25-DOCS release documentation: package acceptance table, roadmap, operator pages and follow-ups — November 13, 2026

Map entries G25-PACKAGES, G25-ROADMAP, G25-OPDOCS, G25-FOLLOWUPS
(`map-g24-g25-release-docs.json`) and G21-13 to G21-16
(`map-g21-route-and-contract-sweep.json`). No decision number; no design
decision was made. Base `7acc069b` (lane `wip/v020/qual`, 7b11a139 plus
G24-LOCAL). Seven source commits: `8951ec53` (acceptance table), `f5925ff2`
(roadmap and progress), `cd93a449` (operator pages), `f13f8ceb`
(follow-ups), and three review fixes: `af69c591` (the `unavailable`
glossary row), `d7ade2fc` (DASH-RPZ and the F rows outside the packages),
`bf156c6b` (a commit for every closed follow-up, F60c's open page). No
generated commit: nothing changes a generator input. No
Go code or test changed.

Each map item was first checked against the files at the base, because
DOCS-ARCH, DOCS-OPS, DOCS-VERSION, DOCS-RECONCILE and LEDGER-SPLICE-2 had
already done most of them. Done before this package and left as they are:
the enrolled cluster guide with activation and promotion, the dynamic
update page, the Kubernetes role-binding and grant-floor text, the TSIG key
page, the managed root anchors page, the Enterprise page, the version
statements of the guides and the migration pages, the command-line chapter,
the capability matrix and refusal glossary (G21-13) and the architecture
corrections (G21-16). Each map grep (`cluster activate`, `cluster promote`,
`rfc2136-`, `kudurru-grant-floors`, `anchorMode` in `docs/deploy`; the
stale phrases of G25-ROADMAP in `README.md` and `docs/architecture`) was
rerun at the base.

What now exists:

- **Acceptance table** (`gaps.md`, "Feature package acceptance at
  154a779a"; pointed to from the completion plan's gate table, the v0.2
  plan, the README and design record §13.2). Eleven rows, V2-ZONE to
  V2-OPS, each with the verdict, its F rows, what is implemented with the
  decision rows and ledger sections that hold the evidence, and what is
  open. No package is accepted: G04 has no green hosted run, G21–G24 are
  open, and the packages not merged at `154a779a` are named (IAM-WRITERS,
  ID-INVENTORY, INT-UNIFI-PUSH, INT-EXTERNAL-ZONES, INT-GROUP-VIEWS,
  CORE-CHILD-ZONE, PUBLISH-STARVATION, HA-OPEN5, HA-OPEN6, F28-OPEN7,
  DASH-RPZ, G21-REFUSAL-OPEN). DASH-RPZ merged on the PR branch after
  `154a779a` (`5b94bd81` to `2918adbf`), so the V2-RPZ row lists the
  dashboard feed views as implemented and the v0.2 plan's current state
  says they are merged. A paragraph under the table names the inventory
  rows that belong to no feature package (F01, F02a, F02b, F02d, F03, F04,
  F10a, F10b, F18a, F23, F33, F51, F52, F55, F56, F63, F64c, F68a and the AD
  rows F11a to F15 except F12a) and says the package assignment is new to
  this table; F21a is listed once, under V2-OPS. The table is written against the PR
  branch head `154a779a`, not the lane base, because that is where it is
  merged; its open items were taken from the tracker, `PLAN.md` and the
  package drafts under `out/`, not from rerunning tests.
- **Roadmap and progress.** README: integrations no longer "planned for
  v0.2"; a v0.2.0 "in development, not released" line; the follower-mode
  shape is the enrolled SQLite cluster with its guide; roadmap and plan
  sentences link the acceptance table. The v0.2 plan: the starting point is
  marked as the October 14 baseline; "Implementation progress" opens with
  the current state, and eleven overtaken sentences now name the decision
  or commit that changed them. Architecture index: the Kubernetes
  integration host and inventory (D384).
- **Operator pages.** The capability matrix says integrations run on
  Kubernetes on the Lease holder, and its `unavailable` glossary row no
  longer gives integrations on Kubernetes as an example; the deployment index links
  `custom-branding.md` and names Kubernetes integrations; chapter 11 points
  to the 0.1.x upgrade notes; the Nodes guide page passes the guide checker.
- **Follow-ups.** 56 rows whose own item is done are deleted and listed
  with their F row and the commit or decision that did the work (every
  one now cites one; the seven that said "inventory row" cite `524d16a8`,
  `f044fcd3`, `0ac2fefb`, `c2b50011`, `66d74761`, `dbfd2963`, `2f362c65`
  and `13dceddd`). The inventory marks those F rows integrated,
  implemented or complete, except F21c, F60b, F60c and F67, which it marks
  active with only package or release gates left; the paragraph says so.
  F60c's open part, the dashboard Integrations page, is kept as its own
  row; its Kubernetes refusals were already in the integration host row.
  Nine partly open rows are restated; seven sections left
  without rows are removed. `grep -c 'v0\.2' dev-docs/followups.md` went
  from 106 to 50 (the rest are open rows, AD preview rows, the
  completion-pass sections and links). Only F64c's target changed, to its
  original v1.0 subtarget.

Evidence under `.context/v020-final-20261110/evidence/G25-DOCS/`:
`guidelint01.log` (`python3 hack/guide-lint.py` at the base plus the
chapter 11 edit: 9 errors, all already present: 7 in
`docs/guide/dashboard/nodes.md`, 2 in `docs/guide/admin/09-people-and-tokens.md`);
`guidelint02.log` (after the Nodes page rewrite: 2 errors, both in chapter 9
line 47, left because the PR head rewrites that paragraph); `checks01.log`
(`git diff --check` clean; `hack/sync-version.py --check` PASS; 14 bench
guard tests OK; every edited table keeps its column count);
`docsgen01.log` (`go test -count=1 ./hack/docsgen` ok); `links01.log`
is empty: that `make docs-check-links` run did not finish and is retained
as such. The reviewer's run at `f13f8ceb`, `review-links01.log`, passed
(168 documents, rc=0), and `fix-links01.log` at `bf156c6b` passed (168
documents, 248 ms, rc=0). `fix-checks01.log` at `bf156c6b`: `git diff
--check` clean, `hack/sync-version.py --check` PASS, guide-lint 67 files
with the same 2 errors in chapter 9 line 47 (this package does not touch
that file, and the sentence with "may" is unchanged at `00cd1b13`), one
sign-off and one Co-Authored-By trailer on each of the seven commits.
`git merge-tree` of the four first commits onto `154a779a` and onto the PR
head `00cd1b13` is clean. `fix-checks02.log` and `fix-checks03.log`:
applied in order onto `00cd1b13`, every commit is clean except
`af69c591`, which conflicts on line 88 of `docs/deploy/capabilities.md`
with `bc5690d8` (the PR head rewrote that row). Resolve it by keeping the
PR head's line and deleting "integrations and " from it
(`sed -i 's/, integrations and API role bindings on Kubernetes,/, API role bindings on Kubernetes,/' docs/deploy/capabilities.md`);
the rest of the commits then apply cleanly.

What remains open. The package verdicts are a documentation statement; the
release still needs G04 green, G21–G24 and the third ledger splice
(D395–D405 rows and sections). Chapter 9 line 47 still fails the guide
checker at the base and at `00cd1b13`; it is left for the merged head.
Closing follow-up rows of packages that are not accepted (their F rows
done, the package gates open) is this package's reading of G25-FOLLOWUPS;
the owner should confirm it. Row-level claims of the acceptance table were
sampled against the tracker, not rerun as tests. The README's v0.1.0 framing (lines 10, 12, 24 of the map) flips
at GA. The map's "hosted docs workflow green" proof needs a push.
