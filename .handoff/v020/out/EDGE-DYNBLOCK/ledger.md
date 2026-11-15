### EDGE-DYNBLOCK Operator edge block push — November 10, 2026 (D388)

An operator can now drop one client address or prefix at every dnsdist edge
until an absolute expiry. `POST /api/v1/edge/blocks` (body `prefix`, and
`seconds` or `until`, optional `reason`), `GET /api/v1/edge/blocks` and
`DELETE /api/v1/edge/blocks/{id}` declare the new `edgeblocks` resource
(`read` for auditors; `read` and `write` for operators and admins; SAR
resource `edgeblocks`). The CLI verbs are `kudurru edge block <prefix>
[duration] [--until] [--reason]`, `kudurru edge blocks` and `kudurru edge
unblock <id>`. A block covers an IPv4 /16 or longer or an IPv6 /32 or longer,
lasts one minute to seven days, and at most 256 are active; a second block on
an active prefix answers 409. The routes are license-exempt operational
controls and primary-only on an enrolled SQLite follower (`primary_required`).

On complete SQLite the command reauthorizes the original request proof inside
the current command, changes one replicated runtime row
(`edge-blocks/v1/state`, validated by the runtime owner checks on replicas)
and writes the audit row in the same transaction. Every member projects the
set of its current publication. On Kubernetes the set is one key
(`edge-blocks.json`) of the release namespace's `kudurru-runtime` ConfigMap,
changed by one UID/resourceVersion CAS write with final authorization and no
retry; refused, unknown and committed outcomes stay distinct, the API audits
the command, and every replica refreshes its observation every two seconds.
Each backend serves the active set on the authenticated internal listener as
`GET /internal/edge/blocks` (owner, revision, ID, prefix and expiry only; the
reason and actor never leave the server).

`kudurru edge run` pulls that route from every backend every five seconds
(whenever `--internal-port` is set and the internal credentials exist), keeps
the newest revision per owner and unions owners, persists the set in its
configuration directory and applies it through the console with dnsdist's
dynamic-block table (`addDynamicBlock`, Drop, remaining seconds), so dnsdist
enforces the expiry itself. A new child receives the whole set; an addition
adds only the new entry; a revoke or changed expiry clears the table and
reinstalls the rest; an unacknowledged statement is reported in `/status`
(`edgeBlocks`, counts only) and resent, without readiness loss. Rendered
configurations and goldens are unchanged.

Each edge remembers, per owner, the revision and blocks it last applied, in
memory and in `edge-blocks.json` (file format 2), across pulls and restarts.
A backend serving a lower revision is ignored and the remembered blocks stay
in force, so a lagging follower cannot bring back a revoked block or wipe a
new one when the newer backend misses pulls. A lower revision is accepted
only after every pull reporting that owner has shown only lower revisions
for `BlockRestoreHold` (five minutes), which covers a restored backup or a
promoted follower that had lagged. A floor is kept for seven days and one
hour after its revision was first applied (longer than any block a lower
revision can carry), and at most eight owners no backend reports any more
are kept. A failed write of `edge-blocks.json` is retried on every pull
until it succeeds. `kudurru edge unblock` refuses an argument that is not a
block ID before any request, escapes the path segment, and honors
`-o json|yaml`.

Evidence under `.context/v020-final-20261110/evidence/EDGE-DYNBLOCK/`:

- `live01-yaml.log`: `TestRunServeEdgeBlockLiveDnsdist` PASS (69.75 s) with the
  YAML renderer against the pinned image `sha256:89c7f196a871…`: the mounted
  SQLite root, its internal listener and a real supervisor. Blocks pushed
  through the API dropped queries from 127.0.0.2 and 127.0.0.4 while 127.0.0.3
  was answered; revoking 127.0.0.4 lifted only that block; a restarted edge
  with no reachable backend reapplied the persisted block; the 60-second block
  was lifted 1.5 s after its expiry.
- `live02-lua.log`: the same test with `KUDURRU_F43_RENDERER=lua`, PASS
  (73.53 s); the block was lifted 0.8 s after its expiry.
- `race09.log`: `TestRunServeEdgeBlocksSQLite` PASS (12.89 s, race): create,
  internal projection at revision 1, 400 for a /8, 409 for a duplicate,
  revoke served at revision 2 with the same owner, 404 for a second revoke,
  `edgeblock.create` and `edgeblock.revoke` audit rows with the admin actor.
- `race07.log`: `TestRunServeKubeEdgeBlocksTwoReplicas` PASS (14.31 s, race,
  envtest): a block created through one replica is served by the other's
  internal listener, a duplicate through the other replica is 409, a revoke
  through it reaches the first; the same log retains a failed
  `TestRunServeEdgeBlocksSQLite` (see below).
- `race03.log`: `TestEdgeBlocksActualCASAndRefusal` PASS with its three
  subtests (envtest: create, coexistence with the blocking annotation, stale
  CAS refused without retry, model refusal not dispatched, malformed state
  refused) next to the unchanged blocking runtime tests.
- `race10.log`: `./internal/api` (including `TestEdgeBlockRoutes`: viewer and
  auditor refusals, request bounds, 409/404 mapping, external audit of both
  commands, 501 without a driver; `TestFollowerOperationsClassifyEveryMutation`
  at 52 primary-only operations; `TestOpenAPIDrift`), `./ee/rbac/...`
  (`TestAggregatedRolesDrift`), `./hack/docsgen` (`TestReferenceDrift`),
  `./internal/edgeblock`, `./internal/runtimeowners`,
  `./internal/cluster/internalapi` (`TestServerEdgeBlocks`,
  `TestEdgeBlockListValidate`) and `./ee/edge/...` (`TestSupervisorEdgeBlocks`,
  `TestEdgeBlockStatementsAndPlan`, unchanged render goldens) all pass with
  race, except `TestSettingsSchemaMetadata` (see below).
- `race11.log`: 28 `./cmd/kudurru` tests PASS with race (97.5 s), including
  `TestCLIReferenceDrift`, `TestForwardedShapes`, the supervisor flag and
  configuration tests, `TestRunServeKubeBlockingTwoReplicasRestartAndOutage`,
  `TestRunServeSQLiteCompletePrimaryUpdateRestart` and both edge block mounts;
  the live test skips there without `KUDURRU_F43_LIVE`.
- `deploy01.log`: `./deploy` PASS after the regenerated chart RBAC.
- `lint03.log`: golangci-lint v2.15.0 over every touched package, 0 issues;
  `lint01.log` and `lint02.log` retain the six `redefines-builtin-id` findings
  (a local named `clear`) fixed in `04b0fd40`. `bench-guards01.log`: 14 guard
  tests OK.

- Review fixes (commits `6436eeed`, `0a046f5a`, `25beaebe`):
  - `fix-03-before.log`: with the new tests and the old supervisor, the new
    `TestSupervisorEdgeBlocks` step fails ("a lagging replica brought back a
    revoked block after the primary missed pulls", as in the reviewer's
    `review-04-lag-probe.log`) and `TestSupervisorEdgeBlocksPersistRetry`
    fails (the file is never rewritten after a failed save).
    `fix-01-before.log` is a retained earlier run whose persist-retry test
    had its own ordering fault (it changed the set before the first block
    was installed); fixed in the test before `fix-03`.
  - `fix-02-merge-after.log` and `fix-04-edge-race.log`: `./ee/edge/dnsdist`
    (including `TestSupervisorEdgeBlocks` with the cross-pull and
    after-restart lagging-follower steps, `TestSupervisorEdgeBlocksPersistRetry`,
    `TestMergeEdgeBlocks` for the floor, the five-minute restore hold, the
    broken-streak rule and the owner memory bounds) and `./ee/edge` PASS with
    race, twice (43.1 s).
  - `fix-05-cli-before.log`: `TestEdgeBlockCLI` fails against the old
    unblock verb (it ignored `-o json`); `fix-06-cli-after.log` PASS with
    race twice, together with `TestCLIReferenceDrift`.
  - `fix-07-kube-mount.log`: `TestRunServeKubeEdgeBlocksTwoReplicas` PASS
    (11.21 s, race) now reading each replica's audit trail and refusing a
    viewer-scoped token with 403 on both replicas; `fix-09-kube-mount-no-audit-wiring.log`:
    the same test with `AuditEdgeBlocks: false` overlaid on `serve.go` fails
    with "create missing from the first replica's audit".
  - `fix-08-live-yaml.log` (62.44 s) and `fix-10-live-lua.log` (62.62 s):
    `TestRunServeEdgeBlockLiveDnsdist` PASS on both renderers against the
    pinned image, now also with an IPv4 /30 (127.0.0.9 dropped, 127.0.0.12
    answered) and an IPv6 /127 on a `::1` listener (::1 dropped); expiry
    lifted 0.8 s after the deadline on both. No test container was left.
  - `fix-11-cmd-race.log`: `TestRunServeKubeEdgeBlocksTwoReplicas`,
    `TestRunServeEdgeBlocksSQLite`, `TestEdgeBlockCLI`, `TestCLIReferenceDrift`
    and `TestRunServeStaticTwinsReadOnlyPeerLossRestart` (the refactored
    `startStaticMount`) PASS with race, twice each (live test skipped there).
  - `fix-12-lint.log`: golangci-lint v2.15.0 over `./ee/edge/dnsdist/...` and
    `./cmd/kudurru/...`, 0 issues. `fix-13-bench-guards.log`: 14 tests OK.
    No generated artifact changed.

Retained failures: `race02.log` is a run whose envtest assets were not
exported, so the kube store tests skipped (rerun as `race03.log`).
`race04.log.note`: a run killed by the agent's own background time limit while
it waited for the heavy lock; it never started. `race06.log`: the SQLite mount
read the internal route before the first complete publication (503) and the
Kubernetes mount used the credential-less fixture kubeconfig without the
authenticating proxy the blocking test uses ("cluster identity could not be
verified"); both were test fixture faults. `race07.log` and `race08.log`: the
SQLite mount read the route while a new publication superseded the handler's
lease (a transient 503, which an edge treats as "keep the last set"), and
then `GET /audit` answered 402 because the fixture license lacked the audit
feature. `race05.log` and `race10.log` retain `TestSettingsSchemaMetadata`
failing in `./internal/api`: it expects four `upstreams.groups` fields and
OBS-TRACING (`e881e6da`) added a fifth (`tracePropagation`); it fails on this
lane's base commit too and is not changed here.

What remains open: the Clients-page action ("block this client at the edge")
is a later dashboard lane. An enrolled follower refuses the commands instead
of forwarding them to the primary (no member command was added). Revoking a
block clears dnsdist's whole dynamic-block table and reinstalls the operator
blocks, so clients that the `--dynblocks` rate rules had blocked get through
until they exceed the rate again; dnsdist has no per-entry removal. The
lagging-replica guarantee has one bounded residual: if the primary stays
unreachable for more than five minutes while a follower keeps serving an
older revision, the edges take that revision as a restore and its blocks
return until they expire; conversely a deliberate restore reaches the edges
only after five minutes (or after deleting `edge-blocks.json` and restarting
the edge). An owner memory evicted past eight absent owners loses its floor.
The final authorization inside the Kubernetes command (a grant revoked
between the middleware check and dispatch) has no refusing test: the mount
refuses an ungranted token in the middleware first, and the store envtest
only counts that the admission runs before the write. The live acceptance ran on this shared machine, not
hosted CI. The full `./cmd/kudurru` suite and `test/e2e` were not run.
