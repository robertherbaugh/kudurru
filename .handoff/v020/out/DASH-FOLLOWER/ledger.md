### FWD-UI Dashboard follower role, primary-only gating and refusal texts — November 10, 2026 (D368)

The dashboard now treats an enrolled SQLite follower as a capability, the way
it treats `readOnly` in static mode. `useClusterRole` (next to `useStaticMode`
in `dashboard/src/api/context.tsx`) reads `role`, `primary`, `primaryRequired`
and the `forwardedOperations` and `localOperations` lists from the node's own
`GET /cluster`. `useCan` refuses a permission whose every mutating operation is
in neither list: users and role bindings, unblock decisions and deletion, list
refresh, Send NOTIFY, TSIG keys, integrations, edge blocks and node log
settings. The permission-to-operation table (`dashboard/src/lib/follower.ts`)
covers only permissions that gate no read, so a follower never hides data, and
a test keeps it equal to `x-kudurru-permission` in `api/openapi.json`. Actions
under a mixed permission ask `usePrimaryOnly` with their own operation ID: allow
and deny entries (Allowlist, Denylist, Domains, the query-log row actions, a
client's Allow/Deny and the Lists page's "Add domains"; the `?add=` deep link
no longer opens the add dialog where entries cannot be written), a block list's source
entries, imports, "Track previous imports" and decommissioning. Every disabled
control's tooltip names the primary. On the Requests list, Approve and Deny are
shown disabled with that tooltip on a follower (to a user who may decide),
instead of being hidden. The authenticator panel says that setup
and removal run on the primary. Everything the follower forwards (objects,
zones and records, lists, groups, policies, settings, blocking, sign-in and
sign-out, own password and tokens) and runs locally (cache flush, DHCP, query
log, dig, promotion) stays enabled. `GET /cluster` needs `cluster.read`, which
only the built-in admin role holds, so for every other session (viewer,
auditor, operator, zone-admin) or when `/cluster` fails, `useClusterRole`
takes the role and the primary from the public `GET /auth/status` and the
forwarded and local lists from `DOCUMENT_FORWARDED` and `DOCUMENT_LOCAL` in
`lib/follower.ts`, a build-time copy of `x-kudurru-follower` that a test keeps
equal to `api/openapi.json` (the dashboard is embedded in the binary, so the
copy describes the same release). Until one of the two reads answers, nothing
is gated and the server decides. The role is reread every 30 seconds
(`ROLE_POLL_MS`) and at once when a write answers `primary_required`, so a tab
left open follows a promotion or a demotion.

A banner above every page names the primary and says what is sent there. The
Nodes page shows the node's role, the primary, "Object writes: forwarded to
<primary>" and how many kinds of change are forwarded or run locally, and it
labels the write authority "primary" on the SQLite driver. The login page of a
follower says that single sign-on runs on the primary (from the `role` and
`primary` of `GET /auth/status`). `errorText` and `ErrorBanner` turn
`503 primary_required` into "Needs the primary", with a sentence naming the
primary and keeping a specific hint such as the one for node log settings, and
`503 completion_unknown` into "Outcome unknown", asking to reload and check
before trying again. The Settings page formats a failed save with
`errorText` too. A sign-out that the server answered but could not confirm
still ends the browser session, as before. That matches the follower, which
clears the cookie also when it cannot revoke the session (FWD-LOGOUT). The hook
no longer rejects silently, though: the login page keeps the server's
explanation that the session may stay valid until it expires, and the notice
clears at the next sign-in. A sign-out that never reached the server (network
failure, `ApiError` status 0) keeps the session, because nothing cleared the
cookie, and rejects; the account menu shows "Sign-out failed: ..." as a toast.
This splits the map's "keep the session when sign-out fails": it is kept only
where it is in fact still usable from this browser (integrator: record in the
decision log if you agree). For read-after-write, the API client reports every
POST, PUT, PATCH and DELETE that the server answered (`ClientOptions.onMutation`,
wired in `ApiProvider`), so it covers `useMutation` and plain awaits alike (the
policy, group and list wizards, zone settings, content categories). After a
success on a follower the active queries are refetched once more six seconds
later (the follower's default 5 s pull interval plus a margin; one timer per
query client, reset by each further write). The Settings page reads node
values with the plain `node.config` permission, so they stay visible on a
follower, where the schema already marks node keys read-only.

Evidence under `.context/v020-final-20261110/evidence/DASH-FOLLOWER/`:

Review fixes (commit `fix(dashboard): follow the follower role for every
session and write`):

- New tests in `src/lib/follower.test.ts` ("keeps the build-time follower lists
  equal to the document, for sessions without cluster.read") and
  `src/api/follower-mode.test.tsx`: an operator session (no `cluster.read`,
  `/cluster` answers 403) on a follower sees the banner and a disabled
  Approve on the Requests list with the primary named; the hook gating from
  `/auth/status` matches `/cluster`; no banner when `/auth/status` names no
  role; the read-back fires for a plain `client.request` write (fake timers)
  and, mounted, after the policy wizard created a policy on a follower (one
  more `GET /objects/policies` about six seconds after the write, none at
  three seconds); the role is reread at once after a `primary_required`
  answer and by the 30 s poll; Approve, Deny and Delete on a request,
  Send NOTIFY on a zone (Add record stays enabled), "Refresh now" on a list;
  Settings keeps reading `/node/config` on a follower; `?add=` does not open
  the dialog on a follower; a Settings save answered `completion_unknown`
  shows "Reload to check what is in effect before you try again."; a
  sign-out that never reached the server keeps the session and rejects.
- `fix-vitest01.log`: follower.test.ts + follower-mode.test.tsx, 28/28 in
  25.9 s (before the sign-out network case was added; it passes alone).
- `fix-vitest02-before-fix.log` (retained failure, on purpose): the new tests
  run against the previous source files: 8 of 23 failed (every new behavior
  except the per-surface gating cases, which the first commit already
  implemented and now pins).
- `fix-typecheck05.log`: `tsc -p tsconfig.json --noEmit`, exit 0
  (`fix-typecheck03.log` and `fix-eslint04.log`, retained, show one unused
  import in the test, then removed). `fix-eslint06.log`: scoped ESLint on the
  nine changed files, exit 0.
- `fix-vitest07-touched.log` (retained failure): src/api, follower,
  Requests, Domains, Settings, Policy, Shell and AccountMenu test files, 155
  passed, 1 failed: Shell.test.tsx "starts expanded and keeps the icon rail
  after navigating" (the `Tools` heading not found within 5 s). Alone it
  passes (`fix-vitest08-shell-1.log`, `-2.log`, 3/3 each); with timings, the
  test takes 4209 ms on this change (`fix-vitest09-shell-head.log`) and
  4194 ms with `dashboard/src` of the base commit f6afe7d6
  (`fix-vitest10-shell-base.log`), so it is a pre-existing near-timeout
  flake on the first lazy page load under load (the same that the reviewer
  saw), not a regression. The test was not changed.
- `fix-vitest11-full.log` (retained failure): full suite through the heavy
  wrapper at a machine load of about 15, 946 passed, 3 failed, 264 s. All
  three hit the 20 s test timeout: the known `AccessPage > lists the audit log
  newest first and pages with the cursor` (26.2 s), `App > signs in locally
  and lands on the dashboard` and `PolicyWizardDialog > walks Who → What → ...
  and creates the policy`. Shell.test.tsx passed in this run. Alone, App.test
  and PolicyPages.test pass 64/64 (`fix-vitest13-app-policy.log`). Timed alone,
  the two tests take 7.3 s / 13.2 s and 6.1 s / 11.0 s with this change
  (`fix-vitest14-timing-head.log`, `fix-vitest17-timing-head.log`) and
  6.5 s / 10.2 s and 6.8 s / 11.1 s with `dashboard/src` of the base commit
  (`fix-vitest15-timing-base.log`, `fix-vitest16-timing-base.log`): no
  measurable change, so these are load timeouts, not regressions. A full run
  on a quiet machine is still needed before the cherry-pick.
- `fix-lint12-full.log`: `bun run lint` (ESLint `--max-warnings 0` over the
  whole dashboard plus the theme check), exit 0.

Original implementation:

- `install01.log`: `bun install --frozen-lockfile`, 359 packages.
- New tests: `src/lib/follower.test.ts` (4 tests: the table equals the OpenAPI
  document; classification equals `x-kudurru-follower` for every operation;
  nothing is gated on a primary, standalone or unknown node; reasons) and
  `src/api/follower-mode.test.tsx` (12 tests: hook gating and reasons on a
  follower and none on a primary; the banner, and none without a role; Lists
  refresh and "Add domains" and the Allowlist switch disabled with the primary
  in the accessible description while "New list" stays enabled; the
  authenticator panel; the Nodes page; the primary_required and
  completion_unknown texts and titles; an unconfirmed sign-out notice on the
  login page that clears after sign-in; the generic session warning; the
  follower login page; the delayed refetch with fake timers, not on a primary,
  once after `FOLLOWER_SYNC_MS` on a follower).
- `vitest01.log` and `vitest02.log` (retained failures): full suites, 7 and 6
  failures. Four `RequestDetailPage` tests failed because `useCannotReason` was
  called after an early return (a hook-order bug in this change), and
  `ExternalUsers` "refuses writes for a read-only caller" failed because a
  tooltip turned `disabled` into `aria-disabled`. Fixed: the hook moved above
  the returns, and the external-account buttons only get the follower tooltip
  (`usePrimaryOnlyPermission`). `vitest03-requests-external.log` shows the
  failures in isolation, and `vitest04-fixed.log` shows the four affected files
  passing (50 tests).
- `vitest05-full.log`: full suite 935 passed, 1 failed, 222 s. The failure is
  `AccessPage > lists the audit log newest first and pages with the cursor`,
  which timed out at 20 s under full-suite load (it also failed in vitest01
  and 02). It is pre-existing and unrelated: alone it takes 15.1 s with this
  change (`vitest08-audit-mine.log`) and 15.8 s at the base commit with the
  change stashed (`vitest09-audit-base.log`).
- `lint06-full.log`: `bun run lint` (ESLint `--max-warnings 0` over the whole
  dashboard plus the theme check), exit 0. `eslint02`/`eslint03`: scoped runs
  while fixing (the retained eslint02 shows four unnecessary type assertions in
  the new test, which were then removed). `typecheck07.log`: `tsc --noEmit`, exit 0.
- `build10.log`: `bun run build`, exit 0 (483.2 KiB transferred of the 600 KiB
  budget). `dashboard/dist/index.html` was restored, not staged.
- `e2e11-build.log`, `e2e12-playwright.log`: `make build GOFLAGS=-tags=licensetest`
  and Playwright `login.spec.ts pages.spec.ts access-sessions.spec.ts` against
  a real `kudurru serve` (standalone): 79 passed in 1.7 min. No follower
  Playwright scenario exists. The follower states are covered by the vitest
  tests against MSW, not by a real enrolled pair.

Open: the Shell.test.tsx first test runs close to the 5 s `findBy` limit at
the base commit as well and fails under full-suite load now and then, and the
long App, PolicyPages and AccessPage tests time out at 20 s under heavy load;
no clean full run was obtained on this shared machine, and no existing test's
timeout was raised (left to the integrator). The dashboard build and
Playwright were not rerun after the fix commit. No browser test runs against a real enrolled follower (test/e2e has the
two-process cluster but no dashboard driver). The banner's list of what is
forwarded and what needs the primary is fixed wording in the UI that follows
D368, while the gating itself follows the node's lists. The delayed refetch is
a fixed six seconds and does not follow a non-default `pull interval`. For a
session without `cluster.read`, gating uses the classification this dashboard
was built with rather than the node's own lists, and the Nodes page's
forwarded/local counts (an admin-only `/cluster` view) stay hidden; the
banner, the tooltips and the gating are the same. Forwarded
object and zone writes still have no server-side pull after commit (the map's
alternative; only settings, login, logout and self-service pull). The CLI part
of FWD-UI was already done by HA-REFUSAL (hints for `primary_required` and
`completion_unknown`, a ROLE column, and LEADER naming the primary on SQLite);
no separate PRIMARY column was added.
