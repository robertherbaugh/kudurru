### F28-GUARDS managed trust-anchor guards, image-less refusal and mixed-version position — November 10, 2026 (D370)

Covers map entries F28-GATE-1, F28-EF-2, F28-EF-1, F28-MV-1 and F28-MV-3 (operator text; the decision text is D370).

**Kubernetes refuses managed anchors (F28-GATE-1).** Config validation now refuses `upstreams.recursion.anchorMode: managed` unless `store.driver` is `sqlite`, and the error names the remedy. Before, the Kubernetes driver accepted the setting and built the resolver from the seeds as plain static anchors, so it promised RFC 5011 and did nothing. On the Kubernetes driver the validation runs at startup, on every settings reload through the settings manager (`settings.Manager.Refresh`, which the settings watcher calls for the shared settings ConfigMap) and in settings PATCH validation, which refuses with 400 before anything is stored. A refused reload keeps the last good configuration and section status. The schema help, the generated configuration and CRD references and the architecture page now say that managed anchors are SQLite-only.

**Image-less nodes get an actionable refusal (F28-EF-2).** Two kinds of start now fail with one specific error. The first is an unenrolled receiver started with `managedTopology: direct` or `edges`: a join that never installed its first image, whether before or after the primary accepted it. The second is any node without an installed image started with `edges`. The error names `kudurru cluster join --offline` and `kudurru cluster join-reconcile --offline` and points to the new runbook. Before, these starts failed with the generic "root anchor policy unavailable or changed", or with a raw store error ("replication: read only: initial image not installed") in the edges lane. The refusal comes before any edge observation or root fetch and writes nothing, and a test shows this through the real `runServe` path, not only the serving runtime. That keeps D319: there is no waiting lane and nothing is recaptured, because the offline join (D355/D356) is the only first-image lane. A direct follower enrolled without an image keeps its existing, tested wait for installation. It contacts no edge, and no production path creates such a follower. A new runbook, "Add a follower behind managed edges", gives the order: edge backends, offline join or join-reconcile, host settings, start.

**Joined follower behind managed edges (F28-EF-1).** A new test follows the supported order in process. A primary bootstraps managed trust through its elected writer. The receiver is joined with the real offline `cluster join` CLI, and its first image carries the managed owner. The follower then starts with edges topology. Each edge reports the runbook's fleet: the primary's backend and the follower's. Startup authenticates the two-edge fleet once and adopts the observation. An edge that lists the follower's backend zero times, twice, or with another internal address refuses the start after the first edge, before any root acquisition, with the follower's authority unchanged. The deny-only supervisor makes exactly one root acquisition, and the runtime publishes the accepted managed root through the production output guard. After a physical restart the follower observes the whole fleet once more and serves the same owner.

**Format guards (F28-MV-1).** New golden documents are committed in the tree:

- pure format 1 and format 2 (the bytes the recovery commit wrote on November 8; the pre-recovery codec of be4446fc re-encoded the format-1 bytes unchanged);
- owner envelope rows: a dormant root, plus managed roots wrapping each pure golden;
- a managed local carrier.

The new tests show that:

- decode then encode is byte-identical, and the version digests are pinned;
- unknown or relabeled pure formats (3, 0, string, format 1 with recoveries, format 2 without) are refused, and so are unknown envelope, chunk and local formats;
- a receiver refuses an image whose envelope, chunk or pure document has an unknown format, and its image, authority and local carrier stay unchanged; a reframing control, and the unmodified image installing afterward, show that each refusal comes from the format alone;
- a durable local carrier in an unknown format is refused when read.

A tripwire test pins the tuple: store schema 9, the nine required features, pure format 1, recovery format 2, envelope format 1, local carrier format 1 and the row namespaces.

**Mixed-version operator text (F28-MV-3).** The recovery runbook step and the `kudurru anchors` help now tell operators what to check: every member runs v0.2.0 or later. They also say that the only earlier release, v0.1.0, has no cluster replication and cannot be a member. A first draft said that older members refuse v0.2.0 images by schema, which no released binary does; the review round removed it. The architecture page replaces "Full mixed-version fleet operational acceptance remains separate" with the position and its evidence. Recovery format 2 was added at schema 8 (c4b39bd9), before schema 9 (9fd961e3). No release contains either commit, and v0.1.0 (schema 1) has no managed anchors, so no format-1-only binary was released.

Evidence under `.context/v020-final-20261110/evidence/F28-GUARDS/`:

- `config-race01`: `go test -race ./internal/config`, ok in 23.8 s. It includes `TestManagedRootAnchorsRequireSQLiteDriver` (four driver and mode cases plus the cluster document) and `TestManagedRootAnchorsKubeReloadKeepsPrevious` (a real `WatchSettings` reload on the Kubernetes driver).
- `pure-race01`: `go test -race ./internal/trustanchors/...` (five packages), all ok, 30.2 s for the slowest. It includes `TestCodecGoldenFormatsAreByteStable` (2 subtests), `TestCodecRejectsUnknownFormat` (7), `TestOwnerGoldenRowsAreByteStable` (3) and `TestOwnerRejectsUnknownFormat` (3).
- `store-race01`: `go test -race ./internal/store/sqlite ./internal/store/replication ./internal/config`, all ok (sqlite 386.1 s). It includes `TestAnchorLocalGoldenRecordIsByteStable`, `TestAnchorFormatGateTuple` and `TestAnchorInstallRefusesUnknownFormatsUnchanged` (4 subtests, 0.91 s in an earlier focused run).
- `race03`: `-race -run '^(TestAnchorNoImageStartupRefusesWithRemedy|TestAnchorEdgeActivationJoinedFollower)$' ./cmd/kudurru`, ok. `TestAnchorNoImageStartupRefusesWithRemedy` passed in 3.07 s, with six subtests: unenrolled, incomplete-join and empty-follower, each under edges and direct. `TestAnchorEdgeActivationJoinedFollower` passed in 4.69 s. No skips.
- `race04-prechange`: the same EF-2 test with `serving_anchor_activation.go` reverted to the lane base. It fails as expected: five of six subtests got "root anchor policy unavailable or changed" or "replication: read only: initial image not installed", and only the documented direct empty-follower wait passed.
- `race05`: anchor regression selection `-race -run '^(Test.*Anchor|TestManagedTopology|TestManagedEdges|TestClusterJoin|TestCLIReferenceDrift|TestOfflineAnchor)' ./cmd/kudurru` on the final head, ok in 330.5 s. It was not run with `-v`, so there are no per-test counts.
- `drift01`: `TestOpenAPIDrift`, `TestReferenceDrift` and `TestAggregatedRolesDrift`, plus `./deploy/...`, `./internal/config/crdschema` and `./internal/store/validator`, all ok after the generated commit.
- `gen01`: `make generate` and `make docs-gen`. `lint02`: scoped golangci-lint v2.15.0 over cmd/kudurru, internal/config, internal/trustanchors and internal/store/sqlite, 0 issues.
- Review-fix round (November 11):
  - `fix-settings01`: `-race -count=2 -run 'TestKubeRefreshRefusesManagedRootAnchors|TestSettingsPatchRefusesManagedRootAnchorsOnKube' ./internal/settings ./internal/api`, both ok and passing both times (0.37 s and 0.39 s; 1.21 s and 0.77 s). The first drives `settings.Manager.Refresh` on the Kubernetes driver, the path the settings watcher uses there: a shared UI-owned upstreams section switched to anchorMode managed is refused, and the configuration and section status stay the last good ones. The second sends a kube `PATCH /api/v1/settings` with anchorMode managed and gets 400 naming the driver requirement, with nothing stored.
  - `fix-settings02-prechange`: the same two tests with the driver check disabled. Both fail: Refresh applied `upstreams.recursion.anchorMode`, and the PATCH returned 200 with `persisted: true`.
  - `fix-race01`: `-race -count=2 -v -run '^(TestRunServeAnchorNoImageRefusesBeforeContact|TestAnchorEdgeActivationJoinedFollower|TestAnchorNoImageStartupRefusesWithRemedy|TestDirectAnchorEnrolledFollowerWaitsWithoutInitializing)$' ./cmd/kudurru`, ok in 24.4 s, all passing both times with no skips. `TestRunServeAnchorNoImageRefusesBeforeContact` (2 subtests, direct and edges) took 2.49 s each time; `TestAnchorEdgeActivationJoinedFollower`, now with a two-backend fleet and three refused memberships, took 5.05 s and 5.12 s; `TestAnchorNoImageStartupRefusesWithRemedy` (6 subtests) took 2.66 s and 2.84 s.
  - `fix-race02-prechange`: the two changed cmd/kudurru tests with the image-less check and the exactly-once membership check disabled. Both fail: runServe returned the generic "root anchor policy unavailable or changed" for direct and edges, and the joined follower started behind an edge that did not list it.
  - `fix-gen01`: `make docs-gen` for the reworded `kudurru anchors` help (only `docs-site/content/reference/cli.md` changed).
  - `fix-lint01`: scoped golangci-lint v2.15.0 over cmd/kudurru, internal/settings and internal/api, 0 issues. `fix-drift01`: `TestCLIReferenceDrift` and `TestReferenceDrift` under race after the new generated commit, both ok.
- `golden-gen/`: the throwaway generators, which were not committed, and their logs for the owner rows and the local carrier.

Retained failed runs:

- `race01`: the unenrolled fixture called `BindAnchorLifecycle` without a deadline ("retirement: invalid input").
- `race02`: `OpenExisting` requires a deadline of at most 30 s. A second accepted join reused the invitation's fixed address and was refused; templates and copies replaced it.
- `lint01`: four gosec G304/G703 findings on test file reads and one ST1008 finding; fixed in a separate commit.

What remains open:

- F28-EF-1 stays open. The opt-in pinned dnsdist test `TestRunServeManagedEdgePinnedDaemonFollower` (KUDURRU_ANCHOR_EDGE_LIVE=1, two backends) was not written or run, because no Docker daemon is available on this machine. EF-1 is therefore proven in process only: authenticated edge peers that report a two-backend fleet, but no dnsdist, no UDP/TCP exchanges through either pool, no edge child restart after adding the backend and no cached-answer check. Runbook step 1 is documented but has not been exercised. The live test belongs next to `TestRunServeManagedEdgePinnedDaemonRestart` and must run where the pinned dnsdist image and a Docker socket are available before "initial empty edge-follower installation" is credited.
- The direct empty-follower case keeps waiting instead of being refused as the map suggested, because an existing test and the architecture text pin that behavior.
- Outside this package: an unenrolled receiver started with the default `managedTopology: disabled` still fails `runServe` with the opaque `providerhost: invalid input` (seen in the reviewer's probe), and `CHANGELOG.md` line 194 still says "initial empty edge-follower installation remain separate", which remains true while EF-1 is open.
- Released-binary mixed-version process acceptance (F28-MV-2) belongs to the upgrade lane.
- The full `./cmd/kudurru` package was not run; the integrator runs `hack/go-test.py`.
