### LEDGER-SPLICE-2 decision log, tracker ledgers and changelog for the packages merged after 68a709e2 — November 13, 2026

Commits `163e7ba7`, `432620c3`, `ab00d935` and the review fix `58325bbf` on `wip/v020/qual` (base
`17b7c87a`, the PR branch at `85913663` plus the four RPZ-OPEN commits) record
everything at that head that the first splice did not; no product code or
test changed, and no generated artifact changed.

- `dev-docs/design-record.md` gains D379–D394 and replaces D368, D369, D372,
  D373, D377 and D378 by the latest complete row at the head. D368 is a
  sentence-level merge of the HA-OPEN and HA-SELF rows (both were written
  against the HA-AUTH-SETTINGS row; one overlapping sentence merged by hand).
  D369 is the second F28-HISTORY-RECHECK row with the F28-PROCESS extension
  and the unnumbered F28-EF1-LIVE row appended as labeled clauses; D373 is
  QUAL-UPGRADE-OPEN's with the QUAL-MIXED-VERSION and QUAL-OPEN2 amendments;
  D377 is CORE-RETAINED-SERVFAIL's with the unnumbered
  CORE-STATUS-WRITE-SERVFAIL row appended; D378 is CORE-OPEN3's; D372 and
  D382 are OBS-OPEN4's; D379 and D380 F60C-OPEN's; D383 QUAL-INTEROP's with
  QUAL-OPEN3's clause (5); D384 F09B-OPEN's; D390–D392 RPZ-OPEN's. D381,
  D383 and D385 end with an "Open at 17b7c87a" sentence from their
  verifiers. D370 is unchanged; FLAKE-OIDC-WRITER's fixture rule is not added.
  No row of D395–D403 is written: none of those packages is at this head.
- `completion-plan.md`, `gaps.md` and `followup-audit.md` gain, verbatim and
  identical in all three, a section on what was merged after `68a709e2` and
  how (the owner's batch merge of November 12, the interruptions, the weekly
  usage limit on November 12 and the resume on November 13, the commits the
  integrator wrote), a verification table of all 44 packages, every failed
  hosted CI run of the pushed heads with its tests (`ee76c04b`, `7648c479`,
  `2b8a5150`, `2133f47e`, `291c55a5`, `ecd633b1`, `f6afe7d6`, `b9b66cba`,
  `b6e09f9d`, `3a6ab137`, `85913663`), the two local enrolled e2e failures at
  `71ac05f0`, and the packages that are not at this head; then the 44 package
  sections from `out/<PKG>/ledger.md` in merge order (only the second
  section of F28-HISTORY-RECHECK, whose first section the first splice
  already holds; FLAKE-OIDC-WRITER's final section with a heading that marks
  it as reviewed; the placeholder decision references of two headings
  replaced), each followed by its review and verification state at
  `17b7c87a` from the lane journals and `followups/*.json`. The inventory
  rows F07a, F09b, F12a, F22d, F26d, F28, F34, F39b, F44b, F46, F60a, F60b,
  F60c, F63 and F68b and the gate rows G04, G05–G15 and G21–G25 say what this
  pass added; nothing is closed.
- `dev-docs/followups.md`: the rows of the 68a709e2 section that later
  packages closed are deleted (each named with its package and commit in the
  new section's introduction), two rows are narrowed, and a section
  "v0.2 completion pass: open at 17b7c87a" lists every unresolved or new
  finding, the hosted CI families and the rows QUAL-INTEROP and QUAL-OPEN3
  drafted.
- `CHANGELOG.md`: the `[Unreleased]` bullets move into `[0.2.0]`, whose two
  Fixed sections become one; `[Unreleased]` stays as an empty heading, the
  form `hack/sync-version.py` expects. New Added, Changed, Fixed, Upgrade
  notes and Known limitations bullets; a fix is folded into its feature's
  bullet when both are new in this pass; existing bullets that later packages
  overtook are corrected (forwarded own password and tokens, CLI password
  login on a follower, Delete on Kubernetes, the narrowed D377, zone groups,
  the refused dial answering `unavailable`, sink shutdown and 2xx handling,
  the managed-edge follower test).

Evidence under `.context/v020-final-20261110/evidence/LEDGER-SPLICE-2/`:
`docsgen01.log` and `docsgen02-verbose.log` (`go test -count=1 ./hack/docsgen`:
`TestReferenceDrift`, `TestText`, `TestFlow`, `TestEnvName` pass, exit 0);
`links01.log` (`make docs-check-links` through the heavy wrapper: Hugo build,
htmltest 167 documents passed, exit 0, 7 min 16 s including the wait for the
lock); `pyguards01.log` (`python3 -B -m unittest discover -s hack -p
'test_bench_*.py'`: 14 tests OK); `sync-version01.log`
(`hack/sync-version.py --check`: PASS, so the restructured changelog keeps the
heading form the release tooling rewrites); `hack/test_sync_version.py` 5 tests
OK (not logged to a file). No failed run. After the review: `fix-01-checks.log`
(`hack/sync-version.py --check` PASS, the 14 bench guard tests OK,
`go test -count=1 ./hack/docsgen` ok, `git diff --check` clean, no British
spelling in the added text, and the edited table rows split into their
table's column count: F07a and F60a 4, RPZ-OPEN 5, the followups RPZ row 3).

What remains open. The state paragraphs are written from the journals and
the verify JSON, not from rerunning any test; where a later package is said
to have taken up a finding, its own section says how far it got. RPZ-OPEN is
included although it is not on the PR branch at the time of writing; its
state (review accepted with seven findings, fixed in `17b7c87a`, verified
with nothing unresolved and three new problems: one minor contradictory
import report line for a name answered on two ports, two nits) is taken from
the qual lane journal and `followups/wave3-open.json`. The
splice describes the tree at `17b7c87a`; CORE-COMMIT-GAP (D395), DASH-CLUSTER,
DASH-OPS, the CI-RED-2 test-only commits and every later package need a third
splice, and the D368–D378 rows they change will need replacing again. No gate
is closed and the release is not ready.
