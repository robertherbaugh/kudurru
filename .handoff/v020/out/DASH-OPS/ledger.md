### DASH-OPS Dashboard surfaces for edge blocks, query-log sinks and zone-group scopes — November 13, 2026 (no new decision; follows D371, D372, D385, D387, D388)

The dashboard now has surfaces for four APIs merged in this pass.

**Operator edge blocks (EDGE-1 UI, D388).** Every address row on **Clients**
and every address client page has **Block at edge**. A client known only by a
client ID has none, because edges see addresses only. The dialog asks for the
address or prefix and checks it against the bounds `GET /edge/blocks`
reports (IPv4 /16 or longer, IPv6 /32 or longer). It also asks for a duration
preset or a custom duration between the reported minimum and maximum, and an
optional reason of at most 256 bytes. It then sends one
`POST /edge/blocks` with `seconds`. **Edge blocks** under the client list
shows the active blocks (prefix, expiry, reason, author) with **Revoke**,
which sends `DELETE /edge/blocks/{id}` after a confirmation. The actions need
`edgeblocks.write`, and the list needs `edgeblocks.read`; without it, the
list is neither rendered nor requested. On an enrolled follower,
`useCan('edgeblocks','write')` refuses the actions with the primary named,
because `createEdgeBlock` and `revokeEdgeBlock` are in neither of the
follower's lists (D368 table `PERMISSION_OPERATIONS`). If the server did not
answer, answered 500/502/504, or answered 503 with the Kubernetes "dispatched"
hint, the outcome is unknown: the dialog shows **Outcome unknown**, disables
its submit button, sends nothing again and rereads the list
(`lib/cluster.ts outcomeUnknown`).

**Query-log sink status (OBS-1/OBS-5 UI, D371, D372).** **Nodes** has a
**Query-log sinks** panel (`querylog.read`). It shows `GET /querylog/sinks`
of the answering node: name, type, the **raw** mark of a raw dnstap sink,
queued/bound, sent, dropped, failed attempts and last success. It names the
node and says that every node runs its own sinks. The route carries no
credential, URL or error text. The panel copies only the named fields into
its rows (`sinkRow`), so a field a later server adds is not shown either. It
shows no "last error": the API deliberately has none (D371: error text can
quote rows). The panel points to the node log and `kudurru_sink_errors_total`
instead.

**Zone-group scope (IAM-2 UI, D385).** `can`, `useCan` and `useCannotReason`
take an optional `{zone, zoneGroup}` and count the `zone:<name>` and
`zonegroup:<group>` entries of `/auth/me scopedPermissions`. The zone page
(Add record, Import, Edit, Delete record, Send NOTIFY, Delete zone), the zone
settings section and the zone list's Delete now enable exactly the actions a
zone- or group-scoped user holds on that zone, and disable the others with
the reason. Before, they used cluster permissions only, so a zone admin saw
everything disabled. Actions stay visible and disabled with a tooltip, as
everywhere else in the dashboard; they are not hidden. **Zones** and the zone
page show `zoneGroup`. **Access > Identity > Role bindings** has **New role
binding**: name, namespace, role, a scope picker (Everything / One zone with
the zone picker / A zone group with label-value validation and the groups in
use), and one subject (User, Group, or ServiceAccount with namespace). It
sends one `POST /iam/rolebindings` (RoleBindingDesired) with the same lock
after an unknown outcome. That route has its own classifier
(`roleBindingOutcomeUnknown`): on the SQLite runtime it answers every error it
does not classify, including a commit that was attempted and failed, with the
catch-all 503 `unavailable` of `servingHTTPAuthError`, so a 503 `unavailable`
counts as a refusal only with the hint "requires the complete SQLite runtime"
(the answer it gives before it runs anything). Any other 503 `unavailable`
locks the dialog as **Outcome unknown**; a definite pre-commit refusal such as
"current writable serving generation unavailable" therefore also locks it,
which costs a reread and never a resend. API tokens can also be limited to
`<role>@zonegroup:<group>`.

**Account-command capabilities (D387).** `useAuthCapabilities` reads
`/auth/status capabilities`. Where `factorManagement` is false, the sign-in
page has no authenticator code field, the Authenticator panel says that no
authenticators are offered (and never asks `GET /auth/totp`), and
**Recover authenticator** is not rendered. A server that does not report
capabilities counts as offering both. The dashboard has no local-user create
or password-reset control, so `localUserCommands` gates nothing yet.

The dashboard guide (`docs/guide/dashboard/clients.md`, `nodes.md`,
`identity.md`, `zones.md`, `access.md`) describes the new actions.

Evidence under `.context/v020-final-20261110/evidence/DASH-OPS/`:

- `vitest03.log` (before the review fixes): `src/components/DashOps.test.tsx`, 16 of 16 pass (MSW
  against the shared mock contract). It covers: block from a row with the
  exact POST body `{prefix, seconds: 3600, reason}`; list and revoke;
  rejection of a /8 before any request; one POST only after a network error,
  then locked; `outcomeUnknown` for 0, 409, primary_required and the
  dispatched 503; disabled actions and no list request without the
  permissions; follower gating of block and revoke; no action for a
  client ID; the sink panel with extra `url`, `passwordRef` and `lastError`
  fields in the answer, none of which (including `s3cr3t`) reach the page;
  the sink panel hidden and never asked without `querylog.read`; `can` with
  zone and group grants; Add record enabled for a labeled member zone and
  disabled once the label is removed; the group shown on Zones; role binding
  creation with the exact manifest, validation of an invalid group before any
  request, scope options, and follower gating; capabilities hiding the TOTP
  controls and the login code field.
- Review fixes (commit `fix(dashboard): lock the role-binding dialog on a
  catch-all 503`): `fix-vitest03-after.log`, `DashOps.test.tsx` 21 of 21 pass
  (31.6 s). The five new or extended cases: the role-binding dialog answered
  503 `{key: unavailable, message: "authentication operation unavailable"}`
  shows **Outcome unknown**, disables Create and sends exactly one POST; the
  same 503 with the missing-runtime hint shows an ordinary error and leaves
  Create enabled; edge-block create answered 503 with
  `{dispatched: true, outcome: unknown}` and revoke answered 500 each lock
  their dialog after one request; a zone-group member zone enables Import,
  Send NOTIFY and the zone settings Edit button and disables them once the
  label is removed; a group grant holding `zones.delete` enables Delete zone
  on the zone page and the Zones list Delete, which turns off when the zone
  moves to another group. `fix-vitest01-before.log` and
  `fix-vitest02-before-full.log` are the runs before the classifier fix
  (17 of 21): both role-binding cases failed as expected, and two Delete
  cases failed because the fixture zone carries external-dns records
  (delete refusal `managed_by`); the tests now remove that record set first.
  `fix-vitest04-mutation-drop-scope.log`: with the scope argument dropped at
  `ZonesPage.tsx:131` and `ZoneSettingsSection.tsx:37`, two zone-group tests
  fail. `fix-typecheck01.log` and `fix-eslint01.log` exit 0.
  `fix-vitest05-access.log`: `AccessPage.test.tsx` 6 of 6 pass (22.3 s, load
  average about 12), including the audit-log test that timed out at load 25.
- `vitest04-mutation-no-scoped-can.log`: with the scope check removed from
  `can`, the two zone-group tests fail.
- `vitest05-full.log`: the whole dashboard suite with two workers through the
  heavy lock, 991 of 992 pass in 482.9 s. The failure is
  `AccessPage > lists the audit log newest first and pages with the cursor`
  (20 s timeout). `vitest06-access.log` repeats it alone, and
  `vitest07-access-base.log` shows it also times out with this package's
  changes stashed (29.9 s). It is a timing problem of the shared machine and
  not changed here.
- `typecheck02.log` (`tsc --noEmit`) clean; `eslint02.log`
  (`eslint --max-warnings 0 .`) exit 0.
- `guide-lint03.log`: `hack/guide-lint.py` finds 9 problems, all in lines
  this package did not write (`admin/09-people-and-tokens.md:47` and
  `dashboard/nodes.md` lines 30-60 from earlier packages). `guide-lint01.log`
  and `guide-lint02.log` retain the runs before this package's own lines were
  shortened.
- `e2e02.log`: Playwright `e2e/operations.spec.ts` PASS (23.4 s). It runs
  against a real `kudurru serve` (`-tags licensetest`, its own SQLite data
  directory) with the embedded production bundle. It blocks 127.0.0.1 from
  its Clients row; the POST answers 201, the list shows `127.0.0.1/32` with
  the reason, and `GET /edge/blocks` agrees. It revokes the block from the
  list, which `GET /edge/blocks` then shows empty. It sets a Loki sink on a
  closed port through `PATCH /settings` and waits for its failed attempts;
  the Nodes panel then shows the row and the node name, and the page text does
  not contain the sink's address. It creates a role binding with scope
  `zonegroup:prod` through the Identity tab (201); the table shows
  "zone group prod" and `GET /iam/rolebindings` reports the scope. No console
  error appears. `e2e01.log` is the first run together with `pages.spec.ts`
  and `login.spec.ts`: 78 passed, and the new spec failed only at its own
  ambiguous `Name` field locator (it also matched `Namespace`), after the
  edge block and sink steps had passed. The bundle build rewrote
  `dashboard/dist/index.html`; it was restored and never staged.

What remains open: the role-binding route cannot tell a pre-commit refusal
from an unknown commit in its bare 503 `unavailable`, so the dialog locks on
both; the server should distinguish them. Role binding replace and delete need the reviewed
UID/resourceVersion (`GET /iam/rolebindings/{ns}/{name}`) and are not built.
The create form takes one subject. Nothing reports whether a driver offers
role binding commands, so on Kubernetes the button is enabled and the 503
"requires the complete SQLite runtime" is shown in the dialog. A
`capabilities.roleBindingCommands` field in `/auth/status` would let the
dashboard hide it. The sink panel shows only the node that serves the
dashboard, because the route asks no other node. It has no last-error text
by design. Edge blocks have no per-client "blocked" mark in the Clients
table (the list is separate). Actions the scoped permissions do not allow
are disabled with a reason, not removed.
