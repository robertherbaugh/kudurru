### QUAL-OPEN2 what verification left open in the upgrade packages — November 12, 2026 (D373)

This closes the open verify findings of QUAL-UPGRADE-OPEN and
QUAL-MIXED-VERSION and two items from their implementers' remaining lists.

A record set at a name a dynamic update (RFC 2136) owns now says so in its
own conditions on SQLite. The committed primary status reads the dynamic
records of the zone's committed head from the current publication
(`PrimaryStatus.DynamicOwnership`, `PreparedImage.CommittedDynamicOwnership`).
On each primary zone reconcile the leader controller checks the kept
endpoints of every record set against that index with the same
`DynamicOwnership.Conflicts` the compiler uses, and reports each collision
as a `Duplicated` conflict that names the endpoint. A partly served set is
Ready and Degraded/Duplicated. A set with nothing served is not Ready. This
also covers the left-out endpoint of a partly served set, which was reported
nowhere before. Record sets whose conflicts change are re-queued. If the head
cannot be read, the controller keeps what it reported. The Kubernetes path
has no committed primary and is unchanged.

`DynamicOwnership` is now an index by name: it holds the owner names of the
dynamic DNAMEs and every name at or above a dynamic record. A check costs a
few map lookups per label instead of a scan of the dynamic records, so a zone
with many dynamic DNAMEs no longer compiles in quadratic time.

The first operational reload after a complete-serving start applies
`Kudurru/default` over the cluster keys of `kudurru.yaml`. It now logs the
warning `cluster settings in kudurru.yaml are not in effect` for the
differing hot keys, with their stored scalar values now in effect, instead
of `settings applied`. Differing restart-required keys are not reconfigured
by that pass, so they get a second warning that names them and says the
process keeps the values it started with, without claiming a value. List
and object values are not logged. Later reloads still log `settings applied`.

Every publication re-queues every primary zone. The controller now keeps a
zone's dynamic conflicts when its zone view and the committed head version
of the ownership index are both unchanged, so a publication for another
zone no longer re-parses this zone's endpoints.

The awaited-body wiring test no longer races a deadline. Its fixture has no
identity provider: the provider's discovery retry about every ten seconds was
the other preparation that let it pass without the wiring. The test also
fails if a store generation, authority or provider dependency change fired
before the list blocked. In the static twin e2e test, the nil == nil
comparison now asserts the seeded 12h0m0s first. The over-long doc lines are
reflowed.

Evidence under `.context/v020-final-20261110/evidence/QUAL-OPEN2/`:

- probe01.log, probe02.log: diagnostic runs (temporary probe test and
  publisher overlay, not committed) with the wiring line removed. The list
  blocked 10.4 s after the body was stored, from a `providercoordinator`
  signal for the fixture's failing identity provider (stack in probe02).
- wake01-race-wired.log: `TestServingRuntimeWakesWhenAnAwaitedListBodyIsStored`
  -race -count=5, 5 passed (blocked 143 to 164 ms after the body was stored).
- wake02-nowire-negative.log: the same test with
  `o.Lists.OnAwaitedBody(s.publisher.Wake)` removed through -overlay,
  -count=8, 8 of 8 failed at the 10 s bound.
- zonestate01-race.log: `TestDynamicOwnershipConflictsMatchesTheScan` and
  `TestDynamicOwnershipConflictsAtTheSourceLimit` pass under -race (100,000
  checks against 20,000 dynamic DNAMEs and 20,000 other dynamic records in
  214 ms).
- zonestate02-bench.log / zonestate03-bench-old-scan.log:
  `BenchmarkDynamicOwnershipConflicts` 380 ns/op with the index, 12.8 ms/op
  with the previous scan (overlay of the previous projection.go).
- zonestate04-primaryzone-race.log: `./internal/zonestate` and
  `./internal/primaryzone` pass under -race.
- controller01-race.log: `TestRecordSetAtDynamicallyOwnedNameReportsDuplicated`
  and `TestRecordSetWithoutDynamicOwnershipCapabilityIsUnchanged` pass.
- pkgs01-race.log: `./internal/controller` (49 s), `./internal/servingbuild`
  (171 s), `./internal/primaryruntime` (52 s) pass under -race.
- mounted01-race.log: retained failed run of the new mounted test
  `TestServingRuntimeRecordSetAtDynamicNameShowsDuplicated`. Two record sets
  shared one endpoint identity, so the view reported an identity duplicate
  instead (a test design error), and one UPDATE got SERVFAIL. In the same
  run the awaited-body, existing dynamic-collision and no-nameserver tests
  passed 3 of 3.
- mounted02-race.log, mounted03-race.log: retained failed runs, 1 of 5 and
  1 of 8. The UPDATE got SERVFAIL while the controller's first status write
  of the zone was still being aligned by the primary driver.
- mounted04-race.log: after the test waited for the zone's Ready condition
  and driver readiness, and resent a refused UPDATE (G21), -race -count=10
  passed 10 of 10 (2.5 to 2.8 s each).
- mounted05-negative-no-controller-wiring.log: with the zone reconcile's
  `reconcileDynamicOwnership` call removed, 2 of 2 failed. The record set
  said Ready "2 endpoints" and Degraded False.
- reload01-race.log: `TestServingReloadLogsClusterSettingsOverTheFileAsInEffect`,
  the existing `TestServingReload*` tests, `TestSubscribeReloadAppliesHotKeys`,
  `TestControlLegacySettingsFileReload`,
  `TestRunServeAuthControlsCanonicalReloadRestart` and
  `TestRunServeQuerylogReloadOriginalNativePublicationAndTail` pass under -race.
- reload02-negative.log: with the first-pass flag never set, the new reload
  test failed waiting for the warning.
- e2e01-static-twin.log: `KUDURRU_E2E=1` `TestAnchorUpgradeStaticTwinV010` passed
  (55 s, both subtests): seeded 12h0m0s asserted, and the restart after the
  file edit logs the not-in-effect warning for `blocking.refreshInterval`.
- lint01.log: retained, 1 revive issue (unused probe parameter in the new
  controller test); lint02.log after the fix: 0 issues over zonestate,
  controller, primaryruntime, servingbuild, cmd/kudurru and test/e2e.
  controller02-race.log: the new controller tests pass again after it.

- fix-02-controller-race.log: the dynamic ownership controller tests and
  `TestDynamicOwnershipKeepsConflictsForAnUnchangedHeadAndZone` pass under
  -race -count=3.
- fix-03-controller-negative.log: with the stamp check disabled, the new
  test failed 2 of 2 (the conflicts were derived again).
- fix-04-controller-pkg-race.log: `./internal/controller` (35 s) and
  `./internal/zonestate` (24 s) pass under -race.
- fix-01-settings-race.log: `TestServingReloadLogsClusterSettingsOverTheFileAsInEffect`
  (now with blocking.initStrategy), `TestApplyReloadOverFileSeparatesRestartKeysAndLogsScalarValuesOnly`
  and `TestServingReloadUsesPublishedCanonicalSettings`, -race -count=3:
  queued behind the machine lock when this was written; the integrator must
  check its result. fix-05-lint.log: scoped lint, also queued.

What remains open: the restart-required settings fix has not been run yet
(see fix-01 and fix-05 above). A zone whose own committed head changes still
derives its dynamic conflicts again on each publication, the same order of
work as the compiler's per-commit check. the controller and the compiler agree on which endpoints
collide, but the controller's zone-model build still contains the
dynamically owned endpoint. Another record set's endpoint can therefore be
reported as a duplicate of an endpoint that the committed zone leaves out.
The compiler's source envelope still reports a partly served set as
`served` with an empty reason. Its own conditions now carry the report. On
static twins a cluster setting still cannot be changed after the first 0.2
start; only the log now says so truthfully. The new mounted test sends an
UPDATE again after SERVFAIL, which is G21 behavior, still open.
