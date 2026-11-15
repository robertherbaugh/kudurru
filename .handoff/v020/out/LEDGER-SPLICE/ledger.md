### LEDGER-SPLICE decision log, tracker ledgers and changelog for the completion pass — November 11, 2026

Commits `de6cfccb` and `a89ff890` on `wip/v020/docs` (base `68a709e2`) record
everything merged at `68a709e2`; no product code changed.

- `dev-docs/design-record.md` gains D367–D378. Where several packages wrote
  one number the latest complete row is used (INT-DEL-C for D367,
  HA-AUTH-SETTINGS for D368, F28-HISTORY-RECHECK for D369, OBS-HTTP-SINKS for
  D371, QUAL-KUBE-UPGRADE for D373); D374–D376 are the HA-FIX rows in order,
  with D375 as corrected by CORE-APPLY-MULTI. D377 and D378 carry an "Open at
  68a709e2" sentence because their verifiers and the merged-head rerun showed
  the rows claim more than the code does. FLAKE-OIDC-WRITER's draft row (a
  test-fixture rule) is not added.
- `completion-plan.md`, `gaps.md` and `followup-audit.md` gain, verbatim and
  identical in all three, a section on what was merged and how (the owner's
  merge of every lane before verification was complete, red hosted CI runs
  42954844334 on `de0d249a` and 43045349810 on `68a709e2` with their failing
  tests, the local pre-push failures, the owner's exclusion of both Cloudflare
  Workers Builds checks, a per-package verification table and the packages
  that never started), then the 22 package sections from `out/<PKG>/ledger.md`
  (headings numbered; the FLAKE draft heading no longer says "draft"), each
  followed by its review and verification state at `68a709e2` from the
  journals and `followups/*.json`. The inventory rows F07a, F09b, F12a, F26d,
  F28, F44b, F46, F60b, F60c, F61a and F63 and the gate rows G04, G05–G15 and
  G21–G25 say what this pass added and that nothing closed.
- `CHANGELOG.md` `[0.2.0]` gains bullets under Added, a new Changed section,
  Fixed, and new Upgrade notes and Known limitations sections; superseded
  drafts are merged, and warnings for defects the merged code fixes are left
  out. `dev-docs/followups.md` gains "v0.2 completion pass: open at 68a709e2".

Review fixes (one more source commit on top). The review accepted the
package with four minor findings, all fixed in the three ledgers identically:
the F28-HISTORY-RECHECK draft's unfilled `final-lint02` placeholder now
reads "0 issues, exit 0" from `final-lint02.log` (head `90740ec6`), and its
state paragraph says the draft was left unfinished there; the verification
table and state paragraphs now match the journals (QUAL-SQLITE-UPGRADE-2
was rejected on one blocker and two major findings; DOCS-ARCH fixed four
minor findings and one nit, and three nits have no recorded state; the
HA-FIX row names the open data race; the unaddressed nits of
QUAL-KUBE-UPGRADE (two) and INT-DEL-C (one) are listed as open); and the
state paragraphs of QUAL-CI, HA-FIX, QUAL-SQLITE-UPGRADE,
QUAL-SQLITE-UPGRADE-2, CORE-APPLY-MULTI and F28-HISTORY now give the
weakened guards their reviewers listed, with whether each was by
specification, rebutted or restored and whether anyone re-examined it. In
`CHANGELOG.md` the serial bullet is qualified (a name with two DNAME
targets, or two CNAME targets of which the dropped one has the lower TTL,
changes content and gets a new serial), the upgrade notes state the CNAME
TTL and DNAME change, and the followups row now says only that no test
covers the CNAME TTL change. `dev-docs/followups.md` gains a row for the
review nits with no recorded state.

Evidence under `.context/v020-final-20261110/evidence/LEDGER-SPLICE/`:
`drift01.log` (`TestReferenceDrift`, `TestOpenAPIDrift`, `TestCLIReferenceDrift`
over `./hack/docsgen`, `./internal/api`, `./cmd/kudurru`). A mechanical check
confirmed that the appended text of the three ledgers is byte-identical.
After the review fixes: `fix-mirror01.log` (the appended text of all three
ledgers has one md5), `fix-guidelint01.log` (`hack/guide-lint.py`: 67 files,
0 errors), `fix-syncversion01.log` (`hack/test_sync_version.py`, which reads
`CHANGELOG.md`: 5 tests OK), `fix-spelling01.log` (no British spelling in
the added lines); the edit script is `fix-scripts/ledger_fix.py`.

What remains open: the sections of the seven lanes still running (CI-RED,
CORE-APPLY-OPEN, QUAL-UPGRADE-OPEN, DOCS-RECONCILE, OBS-OPEN, HA-OPEN,
INT-OPEN) are not in these files. No statement in the ledgers was rerun by
this package; they rest on the package drafts, journals and retained logs.
The PR body is not updated. The 0.2 work of earlier passes sits under
`[Unreleased]` in `CHANGELOG.md` while these bullets are under `[0.2.0]`, as
the map asked; the two sections should be reconciled before release.
