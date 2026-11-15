### DASH-IAM Dashboard IAM writers and the Integrations check hold — November 13, 2026 (D396)

The Identity tab of the Access page now drives every reviewed IAM writer of
the complete SQLite runtime (IAM-3 UI part, G21-08 dashboard part) instead of
read-only tables. **Local users**: New user (username, display name, first
password that must be changed at the first sign-in), Edit… (display name and
the whole list of local groups), Disable…/Enable…, Reset password…, Delete…,
next to the existing authenticator recovery; each command names the row's
exact `uid` and `resourceVersion` from `GET /iam/users`. **Service
accounts**: create, Edit… (description, disabled), Delete…, and New token….
**Identity providers**: create, Edit… (replace) and Delete…, with the client
secret as an environment-variable or file reference, never a value; a replace
carries the stored labels, operator annotations and the spec fields the form
does not show (claim mappings and rules, provisioning, allowed domains,
audiences). **Role bindings**: Edit… and Delete… of API-owned bindings next to
the existing create; an edit carries the reviewed labels and operator
annotations (the server's replace takes both from the desired document), and
a binding whose scope kind is not cluster, zone or zone group (the reserved
`clientgroup`) is not editable here, on the row and again after the reviewed
read, because the form would otherwise send it back as a cluster-wide grant. Every ServiceAccount, IdentityProvider and RoleBinding
command first reads the item (`GET .../{name}`), shows its UID and
resourceVersion, sends exactly those, and refuses an object whose
`kudurru.io/managed-by` is not `api` before sending anything (the manager
annotation is never sent back). Each dialog sends one request: a 4xx is shown
as a refusal and locks a reviewed command (creates may be corrected and sent
again); no answer, a 5xx or a bare `503 unavailable` (the SQLite catch-all,
which the security-row bound also answers) is an unknown outcome that locks
the dialog and says that at the limit nothing changed; `primary_required`,
`read_only`, `user_management_unavailable` and `iam_management_unavailable`
are refusals. A service account token's secret lives only in the token
dialog's component state (no query or mutation cache, never logged) and is
gone when the dialog closes. The disable, group-change, reset and delete
dialogs and the provider change/delete dialogs state the permanent revocation
record they keep and the shared limit of about 4,000 rows at which deletes,
disables and sign-ins answer 503. Controls are disabled with the reason from
the session's permissions (token issuance also needs `tokens.write`), static
mode, the follower table and `GET /auth/status`
`capabilities.localUserCommands`/`iamObjectCommands` (new in
`useAuthCapabilities`); the built-in admin, kudurru.yaml providers, role
bindings of another manager, multi-subject bindings and bindings of an
unsupported scope kind are disabled with their reason. A service account
managed elsewhere is refused when its dialog's read returns, and a local
account of the configuration file by the server's `409 managed_by`. Role
binding create, edit and delete run through the same `useIamCommand` as the
other dialogs (one request, aborted on close, a reviewed edit locked after a
refusal), with a classifier that also keeps the RoleBinding route's
missing-runtime `503 unavailable` a refusal. The MSW mock
(`dashboard/src/mocks/iam.ts`) implements the writers with strict decoding,
exact review, manager and driver refusals.

The Integrations page item its verifier left is closed: after an uncertain
sync, a Check lifts the hold only when it reports the endpoint reachable and
the credential accepted; a 200 Check reporting either one failing keeps it
(`dashboard/src/pages/IntegrationDetailPage.tsx`, docs/architecture/integrations.md).

Evidence under `.context/v020-final-20261110/evidence/DASH-IAM/`:

- `vitest01-integrations.log`: `src/pages/IntegrationsPage.test.tsx`, 22 of 22
  passed (34.4 s), including the new "keeps the uncertain-sync hold after a
  Check that reports the endpoint unreachable or the credential refused";
  `vitest02-integrations-before.log`: that test against the previous page
  fails (Sync now re-enabled after a `reachable: false` Check).
- `vitest05-focused.log`: `src/components/access` (with the new
  `IamWriters.test.tsx`, 21 tests: create, groups edit, disable/enable, reset,
  delete, stale 409 and unknown 503 never resent, built-in admin, service
  account create/edit/delete/token shown once and absent from the caches and
  page after Done, `tokens.write` gate, managed-elsewhere refusal, provider
  create with a file reference, replace keeping unedited fields, delete,
  validation, role binding edit/delete, refusals on Kubernetes, follower,
  static mode and without permission, driver refusal not shown as unknown,
  answer classification), `AccessPage.test.tsx`, `DashOps.test.tsx`,
  `lib/follower.test.ts`, `src/api` and `IntegrationsPage.test.tsx`: 202 of 202
  passed (60.0 s).
- `vitest03-full.log`: `bun run typecheck`, `bun run lint` (zero warnings,
  theme check) and the full vitest suite: 1091 of 1095 passed. The four
  failures are not from this package: three in `src/lib/integrations.test.ts`
  compare the mock and `publishes()` with the Go sources and already fail at
  the base 229b5466 (the merged UniFi push work made `integrationPublishes`
  include `UniFi.Push.MirrorRecords.Enabled` and changed the UniFi schema;
  this package touches neither); `Shell.test.tsx` "starts expanded and keeps
  the icon rail after navigating" timed out under load and passed alone
  (`vitest04-retry.log`, which also shows the three integrations failures
  again).
- `build01.log` (`bun run build`, initial load 493.0 KiB of the 600 KiB
  budget), `gobuild01.log` (`go build -tags licensetest` of the e2e binary,
  empty: success); `dashboard/dist/index.html` restored, not staged.
- `e2e03.log`: `e2e/access-iam.spec.ts` against the real binary, 2 of 2 passed
  (25.2 s): on its own SQLite node, create `e2e-alice` in the dashboard, sign
  in as it in a second browser and change the first password, disable it (its
  `/auth/me` answers 401, sign-in answers 401), delete it (sign-in 401); create
  `apps/e2e-robot`, issue a token in the dashboard, use it once
  (`GET /auth/me` with the bearer outside the browser: 200,
  `ServiceAccount serviceaccount:apps:e2e-robot`), close the dialog and find
  the secret gone from the page. Retained failures: `e2e01.log` (the forced
  password dialog is an `alertdialog`; the principal name of a service
  account is `serviceaccount:<ns>:<name>`), `e2e02.log` (the Banner title is
  rendered twice in the browser; the spec takes the first). Spec fixes only.
- `e2e04.log`: `access-iam`, `access-sessions`, `access-factors`, `pages`,
  `login` and `integrations` specs together: 86 of 86 passed (3.5 min).

Review fixes (commits d07a0d65 and 90762166):

- `fix-vitest01-before.log`: the three new `IamWriters.test.tsx` role binding
  cases against the previous dialog: 3 failed, 1 passed ("keeps the stored
  labels and operator annotations on an edit, never the manager", "does not
  edit a binding whose scope kind the form does not know", "locks an edit
  after a refusal and after an unknown outcome, each sent once").
- `fix-vitest02-after.log`: `src/components/access`, `DashOps.test.tsx`,
  `AccessPage.test.tsx`: 115 of 115 passed (45.1 s), including the existing
  create tests (unknown 503 locks; missing-runtime 503 is an ordinary error).
- `fix-vitest03-focused.log`: those plus `lib/follower.test.ts`,
  `IntegrationsPage.test.tsx` and `src/api`: 13 files, 205 of 205 passed
  (123.5 s).
- `fix-lint01.log`: `bun run typecheck` and `bun run lint` (zero warnings,
  theme check), exit 0.
- `fix-build01.log`, `fix-gobuild01.log`: dashboard build and the
  `licensetest` e2e binary; `dashboard/dist/index.html` not staged.
- `fix-e2e02.log`: `e2e/access-iam.spec.ts` against the rebuilt binary, 3 of 3
  passed (42.5 s). The new "edits and deletes an API-owned role binding and
  keeps its metadata" (9.8 s) creates `default/e2e-rb` (zone group `prod`) and
  `default/e2e-rb-clients` (`clientgroup` scope) through the API, each with a
  label and an operator annotation; Edit of the clientgroup binding is
  disabled; editing the other in the dashboard sends one PUT (200), and
  `GET /iam/rolebindings/default/e2e-rb` then shows the new subject, the same
  scope, the label, the operator annotation and `managed-by: api`; both are
  deleted in the dashboard with one DELETE (204) each. Retained failure:
  `fix-e2e01.log` (edit and metadata checks passed; the spec's "row gone"
  check matched `default/e2e-rb` inside `default/e2e-rb-clients`; spec fix
  only, it now waits for the row's Delete button to disappear).

What remains open. The Integrations mock and `publishes()` lag the merged
UniFi push rule (three failing `integrations.test.ts` cases at base; a UniFi
instance with `push.mirrorRecords.enabled` publishes, so the page should
require a preview for it): an Integrations-page follow-up, not done here. The
dashboard shows no list of a service account's tokens and cannot revoke one
(`GET /auth/tokens?all=true` and `DELETE /auth/tokens/{id}` exist; the CLI
does it). A role binding with more than one subject is edited with the CLI
only (the form edits one subject). Provider claim mappings, claim rules,
provisioning, allowed domains and audiences are kept on replace but not
editable in the form, and Kubernetes `secretRef` is not offered (no writer
there). Local accounts of the configuration file cannot be told apart from
API accounts in `GET /iam/users` (no manager field), so their actions are
enabled and the server's `409 managed_by` is shown as a refusal. The
revocation-record limit is explained, not prevented: the UI does not know how
close the cluster is to it. A created-then-closed token dialog does not ask
"Close the token?" before dropping the secret. The Playwright spec does not
cover identity providers, the refusal states or a follower; those are proven
against the mock only. Role binding create through the form is covered by
vitest only (the e2e creates its bindings through the API to give them a
label, an operator annotation and the clientgroup scope).
