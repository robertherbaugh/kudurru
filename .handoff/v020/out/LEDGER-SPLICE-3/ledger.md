### LEDGER-SPLICE-3 Decision log, tracker ledgers, follow-ups and changelog for what was merged after 17b7c87a — November 14, 2026 (no decision number)

Lane `wip/v020/qual`, base `7663c957` (the PR branch as pushed on November
14). Docs only; no code, test or generated file changed. Commits:
`f700cb49` (decision log), `707d452a` (the three ledgers, the inventory and
gate rows, the acceptance table), `0a2af32c` (changelog), `2d4e6beb`
(follow-ups), and the review fixes `986755a6` (follow-ups), `41e3b9a7`
(ledgers) and `2c588c2e` (changelog wrap).

What now happens. `dev-docs/design-record.md` has D395–D398 and D400–D404
(D398 with F28-REVOKE-HOSTED folded in; D404 is CORE-CHILD-RECONCILE's row
and replaces D385 item (4)); D369 folds in F28-OPEN6 and F28-OPEN7, D384
INT-OPEN2 and D385 CORE-OPEN4; D375 is HA-OPEN5's complete row with the
integrator's position that it stands, and D381 HA-OPEN6's complete row with
a note that the D399 it names is not at this head; D383, D390 and D392 state
what is open at `7663c957`. D395 and D375 carry the owner-confirmation
marks. D399 and D405 are not written (both reverted). The completion plan,
mirrored verbatim in `gaps.md` and `followup-audit.md`, ends with a
third-splice section (merge order and pushed heads, the D399 revert, the
D405 revert after the bisect, the HA-OPEN6 revert on suspicion and its
restore, F28-OPEN6's back-out and re-application, a per-package
verification table, the hosted CI history with the first green `ci` on
`bae1c9df` and the `docs` and `security` failures fixed at this head, the
known intermittent tests, the three decisions marked for the owner, the
goreleaser snapshot that has not run, the decision rows) followed by the 30
package ledger drafts, each with a "Verification state at 7663c957"
paragraph from `followups/wave3-open.json` and the integrator's log. The
inventory rows F09b, F12a, F18c, F28, F44b, F60b, F60c and F63 and the gate
rows G04, G05–G15 and G21–G25 gained a November 14 sentence. The acceptance
table of the 11 packages now describes `7663c957` under the heading
"Feature package acceptance at 7663c957, November 14, 2026"; an explicit
anchor keeps the old `#feature-package-acceptance-at-154a779a-november-13-2026`
links (root README, `dev-docs/milestones/v0.2/README.md`) working, and the
links in the files of this package point to the new anchor. `CHANGELOG.md`
`[0.2.0]` gained the bullets of 29 packages (superseded bullets left out:
CI-RED-2's follower answer, HA-OPEN5's burst line, CORE-OPEN4's parent/child
variant, F28-OPEN7's carried line, G21-REFUSAL-OPEN's hint that
G21-REFUSAL-MERGE took back; the anchor acquisition metric as one bullet
with five outcomes) and an updated Known limitations list.
`dev-docs/followups.md` has a section "open at 7663c957" and the closed rows
of the 17b7c87a section deleted with their commits; the managed-root-anchors
row keeps the three items F28-OPEN6 and F28-OPEN7 still list as open (the
retire-on-REVOKE-then-promote sequence, an abandoned-term member brought
back through the production puller, the SERVFAIL before a node's first
publication that the samplers only bound at 5 s). The D399 revert bullet of
the splice section, and a new follow-up row, name the references that still
cite D399 and D405 at this head although neither has a row
(`docs/deploy/sqlite-join.md` line 193, `serve_zone_change_mount_test.go`
line 106, `serving_member_update_stale_test.go` lines 326 and 334). The
folded changelog bullets are wrapped at 80 columns with two-space
continuation indents, like the rest of the file.

Evidence under `.context/v020-final-20261110/evidence/LEDGER-SPLICE-3/`:

- `docsgen01.log`: `go test -p 2 -count=1 ./hack/docsgen`: ok (0.07 s).
- `links01.log`: `make docs-check-links` through the heavy wrapper: Hugo
  build and htmltest, 168 documents, passed, rc=0 (`CHANGELOG.md` is mounted
  in the docs site; it has no code fence).
- `syncversion01.log`: `python3 hack/sync-version.py --check`: PASS.
- `diffcheck01.log`: `git diff --check` over the four commits: clean. The
  appended ledger drafts were checked for balanced code fences with each
  fence alone on its line (only G24-LOCAL has one block).
- Review fixes: `fix-links01.log` (`make docs-check-links` through the heavy
  wrapper: 168 documents, passed, rc=0), `fix-docsgen01.log` (`go test -p 2
  -count=1 ./hack/docsgen`: ok, rc=0), `fix-wrap01.log` (a
  whitespace-insensitive word diff of the changelog rewrap has 0 changed
  words; 0 lines over 200 columns remain).

What remains open. Nothing here was verified by a second agent. The state
paragraphs rest on the verifiers' reports and the integrator's log, not on
rerunning their tests; DASH-OPS and DASH-OPEN have no entry in
`wave3-open.json` and are taken from the log ("verify 0/0"). The package
drafts are copied verbatim and describe their lane heads; where the merged
tree differs the state paragraph says so, but a draft sentence that
contradicts the merged tree may remain inside a draft (for example
HA-OPEN6's mention of the D399 section of `access.md`, which the integrator
dropped). No hosted result exists for `7663c957`. D399 and D405 need rows
when D399-REDO and PUBLISH-STARVATION land (or the operator page and test
comments that cite them need rewording), INVENTORY-REGRESS needs a
section, and the PR body is not updated. No gate is closed and the release
is not ready.
