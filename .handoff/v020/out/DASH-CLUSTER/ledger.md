### HA-1 / G21-10 Dashboard cluster page with the join wizard — November 13, 2026 (no new decision)

The Nodes page of a SQLite node (GET /cluster reports a `role`, and the node is
not static) now has a **Cluster membership** panel and a **Root trust anchors**
panel. Membership reads GET /cluster/enrollment and offers, by mode, one of
three wizards in a WizardDialog (dev-docs/milestones/m3/ui.md):
**Activate cluster** on a dormant standalone node, **Invite a member** on the
primary and **Review promotion** on a follower. Each wizard reads its review
once when it opens (GET /cluster/enrollment or /cluster/promotion, never
refetched behind the operator; the follower read-back of D368 now skips review
queries), requires an explicit "I reviewed" confirmation, and sends exactly
the values the operator confirmed. Ticking the box snapshots the review on
screen; "Read again" or unticking drops the snapshot, and the finishing
action validates every earlier step again (`revalidateOnFinish` on
`Wizard`), because the stepper jumps to any step already reached without
passing the review step's Next. A re-read review is therefore never sent
without a new confirmation, and a promotion whose re-read review is no
longer eligible is not sent. The command carries the four enrollment identities with the member name,
internal `host:port` and audit reason; for an invitation also an absolute
`expiresAt` computed on the browser's clock when the operator presses Issue
(2, 5 or 9 minutes: the primary accepts at most ten minutes on its own clock,
so the longest choice keeps a minute's margin for a browser clock a little
ahead); for
a promotion the reviewed `memberID` and `installed` generation passed through
unchanged (revision and epoch stay wire strings), the typed node name and one
`dashboard-<uuid>` request ID per opening. Every command is sent once, and
while it is in flight Cancel is disabled and ✕, Escape and the backdrop do
nothing (`isBusy` on `WizardDialog`), so a committed invitation cannot be
dropped by closing the dialog before its answer. A 409 (the review changed;
for an invitation the lock is titled "The primary refused the invitation"
and also names the other documented causes: a member name or internal
address already used or reserved, or an expiry too far ahead or already past
on the primary's clock) or an answer that leaves the outcome unknown
(`completion_unknown`, `committed_output_unavailable`, a lost response, the
client's 35-second bound, any other 5xx) locks the wizard with an explanation
and rereads the page's state; only definite refusals before a commit (400,
401, 403, 413, 415, 429, and 503 `primary_required`/`cluster_fenced`/
`cluster_conflict`/`generation_pending`) can be corrected and decided again
(`commandOutcome` in dashboard/src/lib/cluster.ts). The invitation is requested
with a plain call (no TanStack query or mutation), kept only in the state of
the step that shows it, shown with the `kudurru cluster join --offline`
command and the Docker Compose changes for the new node (a `member`
subdirectory as data directory because the join refuses an existing one; the
invitation on standard input so the container user can read it), and dropped
when the dialog closes; ✕ and Escape ask "Close the invitation?" first.
Activation ends with a "restart kudurru on this node" banner; promotion with
the committed term. The anchors panel renders GET /cluster/anchors (owner,
keys, workers, last pull, warnings) and, for every state that needs an
operator, the documented action linked into docs/deploy/sqlite-join.md and
runbooks.md: a refused adoption by reason (carried_revocation, recovery_history,
dormant_owner, others), fenced behind its term, conflict, quarantine,
retired or terminal output, a pending local denial, stopped workers, closed
admission and an uncertain clock. Error banners and the sign-in page now name
`cluster_fenced` and `cluster_conflict` and link the procedure. No API, Go or
generated file changed.

Evidence under `.context/v020-final-20261110/evidence/DASH-CLUSTER/`:

- `vitest01.log`: first run of the new `src/components/cluster/ClusterMembership.test.tsx`
  (10 of 20 failed: the tests clicked through asynchronously validated steps
  without waiting, and the follower mock lacked `localOperations`, which gated
  the promotion button; both were test defects, fixed before the next run).
  The fixed file then passed 20/20 (activation with exact reviewed body; refusal
  of a non-dormant review; 409 lock with one POST; definite 400 corrected and
  re-decided; invitation shown once and absent from query cache, mutation
  cache, local/session storage, URL and console spies, discard prompt, fresh
  review on reopen; never-replay after a lost response, `completion_unknown`
  and `committed_output_unavailable` (one POST each); ineligible promotion
  blocked with its reason; promotion with exact generation, node confirmation
  and request ID; promotion `completion_unknown` lock; no panels without a
  SQLite role; refused adoption, fenced-behind-term and quiet anchors; helper
  tests for addresses, outcomes, snippets and anchor advice; the fenced banner).
- `lint01.log`: typecheck and lint, 2 `no-unnecessary-type-assertion` errors
  (fixed); `lint02.log`: typecheck and lint after the commits.
- `vitest02.log`: full dashboard suite, 968 passed, 1 failed in 213 s:
  `AccessPage > lists the audit log newest first and pages with the cursor`
  timed out at 20 s under a load average of about 13.5. Re-run alone it timed
  out with and without this package's changes (at the base commit it failed
  once at 23.9 s and passed once), so it is load-sensitive and unrelated.
  `vitest03.log` is a focused re-run of the touched areas (the same AccessPage
  timeout, everything else passed).
- `build01.log`: `bun run build` (bundle within budget) and a
  `-tags licensetest` binary.
- `e2e01.log`-`e2e04.log`: retained failed Playwright runs of the new
  `e2e/cluster.spec.ts`: the browser context inherited the worker's session
  cookie for 127.0.0.1 (01, 02), a second sign-in after the restart sent a
  session cookie without CSRF (03), and a wrapper timeout before the run
  started (04). All were spec defects.
- `e2e05.log`: `cluster.spec.ts` passed (15.2 s): activate through the
  wizard, restart, the session survives, issue an invitation shown once, no
  invitation in console, request URLs, storage, cookies, server log or page
  after close, and `kudurru cluster join --offline` with the shown invitation
  answers `primary.committed: true` with an installed digest.
- `e2e06.log`: `cluster`, `login`, `pages` and `secondary` specs, 80 passed in
  3.0 min (the changed server helper starts and stops every node).
- `review-*.log`: the independent reviewer's runs (accept-with-fixes).

Review fixes (on top of the two commits above):

- `fix-vitest01.log`: the seven new tests before the fix, with a stepper
  helper that did not find the Astryx step buttons yet (6 failed; test defect).
  `fix-vitest02.log`: the seven new tests before the fix, all failing for the
  intended reasons (no return to the review step after Read again or an
  untick and a stepper jump; a re-read digest sent without a new
  confirmation; a re-read ineligible promotion not blocked; a 10-minute
  option offered; the 409 lock text; Cancel enabled while the invitation
  request is in flight). `fix-vitest03.log`: after the fix,
  `ClusterMembership.test.tsx` 27/27 passed: `sends no invitation after Read
  again on a revisited review step, even through the stepper`, the same
  `after unticking the confirmation`, `sends exactly the confirmed review,
  never a re-read one that was not confirmed again`, `sends no promotion when
  a re-read review is no longer eligible`, `offers invitation lifetimes that
  leave a margin below the primary maximum`, `names the correctable causes
  when the primary refuses an invitation with 409`, `cannot be closed while
  the invitation request is in flight`.
- `fix-lint01.log`: typecheck and lint, exit 0.
- `fix-vitest04.log`: every test file that uses `Wizard` or `WizardDialog`
  plus the cluster, api, Nodes and Login tests: 19 files, 207 passed, 1
  failed in 76 s (11 min wall time behind the lock): `PolicyWizardDialog >
  walks Who → What → Safe search → How → When → Review and creates the policy`
  timed out at 20 s under a load average of about 21. PolicyWizard uses none
  of the new opt-in Wizard props; `fix-vitest05.log` re-ran
  `PolicyPages.test.tsx` alone: 14/14 passed.
- `fix-build01.log`: `bun run build` within budget (490.0 of 600 KiB) and a
  temporary `-tags licensetest` binary (removed afterwards; `dist/index.html`
  restored). `fix-e2e01.log`, `fix-e2e02.log`: `e2e/cluster.spec.ts` against a
  real SQLite node passed twice (25.0 s and 15.7 s). The real-node spec now also
  asserts that the **Root trust anchors** region renders this node's
  `GET /cluster/anchors` answer (Anchor mode, Authority, either the advice
  list or "No trust-anchor state needs you on this node.", and no error
  banner).

What remains open: the invitation expiry is still computed on the browser's
clock, not derived from the primary's; a browser clock more than a minute
ahead of the primary, or behind it by more than the chosen validity, still
gets a 409 (now with a lock text that names the clock). A 409 to an
invitation still locks the wizard even when the cause is a correctable
reused name or address, because the answer does not say which cause applies.
Beyond that: the root-anchor states are shown for the node that serves
the dashboard only. On SQLite, GET /nodes lists only that node and a fenced or
conflicted member closes its HTTP API, so `fencedBehindTerm` and a pull refusal
of a fenced member are proven with vitest against MSW, not against a real
fenced member; the panel and docs say to read such a member with its metrics
or `anchors status --offline`. No browser test drives a promotion (it needs a
lost primary and two enrolled processes; test/e2e has them but no dashboard
driver), so promotion is proven with vitest only. The Compose snippet is not
exercised by a Compose run. kudurru 0.2.0 has no member removal, so the
wizard only notes that earlier names and addresses stay reserved.
