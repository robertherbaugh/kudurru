### G21-11 Refusal inventory: what verification left open — November 13, 2026 (no decision row)

This package closes the open findings of the G21-REFUSAL-INVENTORY
verification (`followups/wave3-open.json`, `verify:G21-REFUSAL-INVENTORY`).

Deployment limits that answered a 503 without a hint now have one. All 501
and 503 answers in `internal/api` were reviewed again:

- Rollup recovery evidence (`rollupEvidence`, `verifyRollupEvidence`) on a
  node without a central database: the hint names `database.dsn` and
  `database.querylog.ship` (docs/deploy/postgres.md). Both operations joined
  `featureRefusals` under the new feature `FeatureCentralHistory`. Their
  probes now send a source and a closed hour from the last day (`{closedHour}`
  is filled in when the probe is sent), so every walk reaches the refusal.
- Peer health of a node that this node cannot reach over the internal listener
  (no internal credentials, or a peer without an internal address). Also
  history and live-tail reads that no requested node answered. Each hint names
  the internal credentials and the query log, and says to ask with
  `scope=node`.
- `listIntegrationManifests` on a server without the integration runtime
  answers the same hinted `errIntegrationsUnavailable` as the other
  integration routes.
- `querySuggestions` joined `featureRefusals` (query log), and its probe
  sends `field=domain`.

The DHCP setup write (`putDHCPSetup`) with no setup manager now says what is
true on each driver. `runServe` opens the manager on every Linux SQLite node,
with DHCP on or off. So only two cases reach this answer:

- A Kubernetes node with DHCP off: the hint is the Helm values `dhcp.enabled`
  and `dhcp.serverAddress`, as `dhcpGetSetup` says.
- A SQLite node that is not on Linux: DHCP runs only on Linux, and no setting
  helps.

The Kubernetes walk now reaches this answer: the probe sends a valid body with
an If-Match that matches nothing. `featureRefusalShapes` limits the DHCP
feature refusal of `putDHCPSetup` to Kubernetes. On the standalone node the
probe answers `412`.

`TestRefusalEmittersExplainThemselves` changes in three ways:

- `hintlessRefusals` is now a map from site key to the number of call sites.
  A second hintless answer that reuses a listed message in the same function
  fails, and so does a second `<dynamic>` message.
- A numeric `501`/`503` counts as a refusal.
- A hint resolves through literals, package string constants and `+`
  concatenation. A status or hint that the check cannot read is reported,
  unless `unresolvedRefusalSites` lists it with its count and a reason. It
  lists two: huma's own error constructor, and `primaryRequired`, which
  replaces an empty hint before it calls `newError`.

`TestRefusalEmitterScanCountsAndResolves` holds the scanner to these rules on
a fixture source.

The fenced shape is now walked between processes. In
`TestEnrolledClusterProcessesPromoteAndRejoin`, the served document and a
session of its own are taken from the former primary before it stops for the
promotion. After `enrolledFencedClosed`, while the partition keeps it fenced:

- `enrolledFencedLoginRefused` asserts that a password sign-in with valid
  credentials answers `503 cluster_fenced`, names the newer primary, carries
  no `Retry-After` and sets no cookie;
- `enrolledRefusalInventory(..., api.ShapeFenced, ...)` walks every operation
  with the earlier session.

`docs/deploy/capabilities.md` now gives both meanings of `not_implemented`.
`CONTRIBUTING.md` describes the counted emitter check.

The lane's HEAD already contained routes and behavior that the inventory
table did not reflect, so the table and two stale tests were brought up to
date. Walks confirmed every change:

- Nine operations had no line: `createEdgeBlock`, `listEdgeBlocks`,
  `revokeEdgeBlock`, `getClusterAnchors`, `getBlockListRPZFeeds`,
  `listRPZFeeds`, `querylogSinks`, `listIntegrationManifests` and
  `batchDeleteIntegrations`. Each has one now. Static answers `read_only` for
  edge block writes and `batchDeleteIntegrations`. Static and Kubernetes
  answer `unavailable` for `getClusterAnchors`.
- Integrations now run on Kubernetes in this lane, so the Kubernetes
  `unavailable` lines of every integration operation were removed.
- `createToken` is `FollowerForwarded` (D368). The follower examples in
  `TestExpectedRefusalInheritance` and `TestProblemsNameWhatToAdd` now use
  `createAllowlistEntries`, a `FollowerPrimary` mutation.
- The emitter list gained the hintless sites that other packages added: the
  integration license capture, the edge block set invariant and the RPZ
  snapshot. Two functions were renamed (`requireGroup`, `writeCommand`).

Evidence under `.context/v020-final-20261110/evidence/G21-REFUSAL-OPEN/`:

- `root01-dump.log`: a retained failure (no race, with dump). The standalone
  walk and both closed members passed (200 operations; fenced and conflicted
  200 of 200 refused). The static and Kubernetes walks failed for the stale
  lines above: integrations on Kubernetes, the anchors and the edge block
  writes. The dump shows `rollupEvidence` and `verifyRollupEvidence` answering
  the hinted 503 on every shape, and the Kubernetes `putDHCPSetup` answering
  "DHCP is off in this release" with the Helm hint.
- `emit01-mutation.log`: two hintless refusals were inserted into
  `recoveryError` and reverted afterward: a second `errUnavailable` with a
  listed message, and a numeric `503`. The counted check reports both ("2
  times, and hintlessRefusals lists 1"; "a numeric status").
- `api01-before.log`: the new handler assertions with the handler changes
  stashed. `TestKubeDHCPOffInTheChart`, `TestDHCPSetupWithoutManagerOnSQLite`,
  `TestNodeHealthDiscoveryAndMissingTransport` (no client, no address),
  `TestRollupRecoveryWithoutCentralDatabase` and
  `TestClusterReadsWithLocalLogDisabled` all fail.
`race02-root.log` is a retained failure that shows nothing about the code.
Its build ran while `emit01` and `api01` had temporarily mutated or stashed
`querylog_recovery.go`, so the recovery refusal had no hint. It was rerun.
- `race03-api.log`: a retained failure. `-race` for `internal/api/...` with
  the default 10-minute package timeout ran out under machine load
  (`TestZoneGroupResolutionReadsEachZoneOnce` was running); no test failed.
  `race06-api.log` is the rerun with `-timeout 40m`: `internal/api` passes
  (735.9 s), and so does `internal/api/refusalinventory`.
- `race04-root.log`: `-race` for the standalone, static and Kubernetes walks,
  the closed members, `TestLegacyAuthHandlerFallbackAndRefusals` and
  `TestRunServeKubeForwardedAuthRoutesAnswerEnvelope`. Everything passed
  except the standalone walk, a retained failure. There, `putPolicies`
  answered the hintless `503 unavailable` "current writable serving
  generation unavailable" (`cmd/kudurru/serving_settings_api.go`, the
  command-path refusal that D399 left open).
- `race05-standalone.log` (`-count=2`): the same failure once, and one pass.
- `race07-exp-no-dhcp-probe.log`: an experiment with the `putDHCPSetup` probe
  removed for that run only. Three of three standalone walks passed. The
  probe records a refused attempt in the audit log. The first version of
  this package then sent it after every other probe (`lateProbes`); the
  review fixes below replaced that reordering with an explained answer.
- `race08-root-late.log`: `-race -count=3` of the standalone, static and
  Kubernetes walks with that order (superseded by `fix-08`): nine of nine
  pass. Per walk:
  - standalone: 200 operations, 17 refused, 29 stopped at validation, 40
    answered 404;
  - static: 93 refused, 7 at validation, 23 answered 404;
  - Kubernetes: 41 refused, 29 at validation, 46 answered 404.
- `e2e01-cut.log` (`KUDURRU_E2E=1`, no race):
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` passed (209.2 s).
  The primary walk had 200 operations: 24 refused, 29 at validation, 40
  answered 404. The follower walk had 73 refused, 13 at validation, 31
  answered 404. The background limit then stopped the run in the middle of
  the promote test, after a long wait for the heavy lock. That cut is not a
  test failure.
- `e2e02.log`: `TestEnrolledClusterProcessesPromoteAndRejoin` passes
  (233.2 s): the sign-in refusal holds, and the fenced walk has 200 of 200
  answering `cluster_fenced`. `TestEnrolledProcessConflictResolution` passes
  (127.7 s; 200 of 200 on each conflicted process).
- `lint01.log`: golangci-lint v2.15.0 for `internal/api/...`,
  `test/e2e/...` and `cmd/kudurru/...` reports 0 issues. `gofmt -l` is empty
  and `go vet` is clean for the touched packages. The bench guards pass (14
  tests). `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestOpenAPIServedAndDocs` and `TestUnknownAPIPathIsEnvelope` pass. No
  generated artifact changed, so there is no `chore(gen)` commit.

Review fixes (four more source commits):

- The command path's `503 unavailable` "current writable serving
  generation unavailable" (`currentCommandAPIError` in
  `cmd/kudurru/serving_settings_api.go`, answered when the node's writable
  state changed while a command was admitted) now carries a hint: read the
  current state, then send the request again if it is still needed
  (`api.CommandStateChangedHint`). It still carries no `Retry-After`. A
  command whose acknowledgment failed after its commit must not be
  replayed blindly, so it is not a `generation_pending`. `lateProbes` is
  gone, and every probe but the session-ending ones runs in path order
  again. The walk now sends a probe again after this answer, as it does
  after `generation_pending`, for at most a minute, and holds an answer that
  lasts the minute to the inventory. `docs/deploy/capabilities.md` lists
  the answer under `unavailable`.
- `featureRefusalShapes` applies only on a Linux host (`hostOS`, which is
  `runtime.GOOS`). On any other host no SQLite node opens the DHCP setup
  manager, so the walks expect `putDHCPSetup` to answer `unavailable` on
  every shape. `TestExpectedRefusalInheritance` holds both a Linux and a
  darwin host.
- A peer health read that reached the peer and failed (down, timeout,
  refused credential, `unsupported` on an older version, invalid answer)
  now carries `peerHealthHint`. The line `nodes.go nodeHealth: %s` left
  `hintlessRefusals`.
- `queryAction` joined `featureRefusals` under `FeatureCentralHistory`. A
  probe can now fill `{id}`. Its probe names row 9007199254740991 with
  `scope=cluster`, so every walk without a central database reaches the
  hinted refusal; with one, the row is not found.

Evidence of the review fixes, in the same directory:

- `fix-01-walk-before.log`: with `walk.go` from before the fixes,
  `TestProbeResendsAfterCommandStateChanged` fails (one send, the 503
  observed), and `TestOperationsKeepDHCPSetupInPathOrder` fails ("putPolicies
  putDHCPSetup logout"). `fix-02-walk-after.log`: all seven walk unit tests
  pass.
- `fix-03-cmd-before.log`: `TestCurrentCommandStateChangedExplainsItself`
  fails with the hint removed from `currentCommandAPIError` (hint "" for
  every cause).
- `fix-04-hostos-before.log`: with the host condition removed,
  `TestExpectedRefusalInheritance` fails for standalone, primary and
  follower on a darwin host. `fix-05-hostos-after.log`: `TestExpectedRefusal*`
  and `TestRefusal*` pass.
- `fix-06-peerhint-before.log`: without the hint,
  `TestNodeHealthPeerFailuresRedacted` and `TestNodeHealthUnreachable` fail.
  `fix-07-peerhint-after.log`: they pass with `TestRefusalEmitter*`,
  `TestRefusal*` and `TestExpectedRefusal*`.
- `fix-08-mounted-race.log`: `-race -count=3` of
  `TestRunServeRefusalInventory*`, `TestRootMemberRefusalInventoryClosedMembers`
  and `TestCurrentCommandStateChangedExplainsItself`: all 15 pass (409.5 s),
  with `putDHCPSetup` back in path order. Per walk: standalone 200
  operations, 18 refused, 28 stopped at validation, 40 answered 404; static
  93, 7, 23; Kubernetes 42, 28, 46; fenced and conflicted 200 of 200.
  `queryAction` accounts for the extra refusal on standalone and
  Kubernetes, one fewer at validation.
- `fix-09-e2e.log` (`KUDURRU_E2E=1`, no race):
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (209.1 s; primary
  25 refused, 28 at validation; follower 73, 13) and
  `TestEnrolledClusterProcessesPromoteAndRejoin` pass (fenced 200 of 200).
  The log holds two runs of the same command, both passing: a first
  invocation was assumed lost and started again, and both wrote to it.
- `fix-10-api-race.log`: `-race` of `TestNodeHealth*`, `TestRefusal*`,
  `TestExpectedRefusal*`, `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestDHCPSetup*` and `TestKubeDHCP*` in `internal/api` passes.
  `fix-12-walk-race.log`: `-race` of `internal/api/refusalinventory` passes.
- `fix-11-lint.log`: golangci-lint v2.15.0 for `internal/api/...` and
  `cmd/kudurru/...` reports 0 issues. `gofmt -l` is empty and the bench
  guards pass. No generated artifact changed, so there is no `chore(gen)`
  commit. The full `-race ./internal/api/...` was not rerun after the
  fixes (`race06-api.log` is from before them).

What remains open:

- `currentCommandAPIError` still gives one message and key to every
  cause it groups (a commit that retired the request's publication, a new
  writer term, a fence, a replaced dependency). A hint now explains that
  answer, but it does not say which of them happened, and it does not
  separate a refusal before the commit from an acknowledgment that failed
  after it. Telling those apart, and answering `generation_pending` with
  `Retry-After` for the first, belongs to the D399 / CMD-RECAPTURE work.
  The static emitter check still does not cover `cmd/kudurru`; the unit
  test covers this one site.

- The table now matches this lane's HEAD. The integration branch may differ:
  it documents Kubernetes integrations as available, as this lane does now.
  After cherry-picking, rerun `TestRefusalInventoryNamesEveryOperation` and
  the walks. Their messages say what to add.
- Walks still stop at request validation for about 28 operations on the
  standalone and Kubernetes nodes, because a valid body would change a shared
  fixture (`createEdgeBlock`, `batchDeleteIntegrations` and others; 28 after
  the review fixes). The counted static check covers those in
  `internal/api` only. Handlers in `cmd/kudurru`, `internal/auth/stack` and
  `internal/servinghttp` have no static check.
- The `getClusterAnchors` hint on static and Kubernetes ("requires the
  complete nonstatic SQLite runtime") is present but terse. It was not
  changed.
- The capabilities matrix row for background integrations on Kubernetes is
  stale in this lane, but the integration branch has the current text. It was
  not touched here, to avoid a conflict. The changelog draft of the
  inventory package is still not in `CHANGELOG.md`; the tracker package
  carries it.
