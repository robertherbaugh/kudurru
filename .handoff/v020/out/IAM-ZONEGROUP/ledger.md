### IAM-ZONEGROUP Effective zonegroup scope for role bindings — November 10, 2026 (D385)

A RoleBinding (or `rbac.bindings` entry) with scope `{kind: zonegroup, name: <group>}`
now grants its role on every DNSZone labeled `kudurru.io/zonegroup: <group>` and on
the record sets whose `zoneRef` names such a zone, on the complete SQLite runtime and
on Kubernetes. Before, the scope was accepted but granted nothing, because
`Scope.Covers` needs an exact kind and name match and routes only present cluster and
zone scopes. Routes still present those scopes. The code that reads the zone of a
request also states the zone's group in `rbac.Attributes.ZoneGroup`, and
`rbac.Scope.Admits` lets a zonegroup scope cover it. All five authorizers use it: the
binding authorizer, the engine's token restrictions and implicit grants, the live
bindings and frozen session grants of `ee/rbac.BindingSource`, and the servingauth
View. The API router resolves the group in `Server.authorize`: from the request's
captured publication on the complete SQLite runtime, from the store on Kubernetes,
only when the decision without it is a denial, at most once per zone per request
(zone lists decide on the listed objects), and a zone that cannot be read answers 503. So
route checks, item checks and list filters (`GET /zones`, `/objects/dnszones`,
`/objects/dnsrecordsets`) all apply the same rule. Every SQLite command
reauthorization (`ReauthorizeCurrent`, `...WithClock`, `...AllWithClock`,
`...ForOutput`, `ReauthorizeOperationCurrent`) discards the caller's group and reads
the DNSZone again in its own transaction. A label or binding removed before the commit
therefore denies the write. That also holds for a follower's forwarded command, whose
early admission uses the follower's captured cut. A zone that is absent or
unlabeled is in no group. A DNSZone write that adds, changes or removes the label
needs `zones.write` at cluster scope, on both drivers. On Kubernetes the store write
decides that rule again on the read it compare-and-swaps against, and a label change
through the API is a forced whole-object takeover with the written label checked, so an
administrator's removal of a label kubectl also owns takes effect (it answered 200 and
kept the label before). `GET /zones` reports
`zoneGroup`. `GET /auth/me` now reports `scopedPermissions` for every scoped grant in
`roles` (before, it reported cluster permissions only). RoleBinding admission, and
`rbac.bindings` at start through the same `Validate`, refuse a zonegroup name that is
not a valid label value. The settings help, the RoleBinding
CRD field description, `docs/architecture/access.md` (new section and diagram) and the
people guide describe the scope. TestBindingScopeHelp replaces
TestReservedBindingScopeHelp, because the "reserved" premise of that test no longer
holds for zonegroup; clientgroup is still checked as reserved.

Evidence under `.context/v020-final-20261110/evidence/IAM-ZONEGROUP/`:

- `race15.log`, `race16.log`: `go test -race -count=1 -run 'ZoneGroup|TestServingObjectCommand|TestRunServeKubeForwardedAuthRoutes|TestServingHTTPTokenOwnerScope' ./cmd/kudurru`, 11 passed, 0 skipped, 40.4 s: TestRunServeSQLiteZoneGroupBinding 7.6 s, TestRunServeKubeZoneGroupBinding (envtest) 7.9 s, TestServingObjectCommandResolvesZoneGroupInTransaction 2.9 s, and the existing object-command, token-scope and Kubernetes auth-route tests. race16 repeats the two mounted tests after the lint fix: 2 passed, 15.8 s. The mounted scenario runs `runServe` with a signed RBAC license. It checks `/auth/me`, list filtering, item reads, record writes, the refused relabeling by the group admin, the loss of zone b after its label is removed and its return when the label is back, and the loss of access after the binding is deleted. On SQLite the session ends (401). On Kubernetes the frozen session grant is refused (403).
- `race07.log`: the same selection; 22 neighbors passed and both mounted tests failed at the admin login (seeding a local user ends first boot). Fixed by seeding a built-in administrator. `race08.log`: `/auth/me` had no `scopedPermissions` on either driver, which led to the `/auth/me` change. `race09.log`: SQLite answered 204 for the binding delete (the test expected 200). Kubernetes kept zone b's label after an API PUT that omitted it: the seeded object belongs to the kubectl client's field manager, and server-side apply keeps labels owned by another manager (existing behavior). The test now relabels with kubectl on Kubernetes. `race10.log`: SQLite ended the session (401) instead of answering 403 after the binding was deleted; the test accepts either.
- `race12-mutation-no-admits.log`, `race14-mutation-no-admits.log`: with `Scope.Admits` reduced to `Covers`, all three cmd tests fail (`GET /zones lists []` on both drivers; the member write is refused in the transaction). `race13` and the first `race14` attempt found that an earlier version of the transaction test passed vacuously when the HTTP middleware refused the request without running the handler; the test now treats that case as not run and asks again.
- `race01.log`–`race02.log`: early versions of the transaction test refused with generation_pending; it now retries refusals that come before admission. `race03.log`, `race04.log`, `race13`: queued for the heavy lock and stopped; no test ran.
- `race05.log`: `-race` on `./internal/rbac ./internal/auth ./ee/rbac`, all ok (TestScopeAdmitsZoneGroup, TestZoneGroupBindingsAndRestrictions, TestZoneGroupBindingTokenScopeContract, TestBindingSourceZoneGroupSessionGrants). `race06.log`: `./internal/api ./internal/servingauth ./internal/store/validator`; servingauth (61 s) and validator ok, api failed only TestOpenAPIDrift before regeneration. `race17.log`: after regeneration, `-race` on `./internal/api` (174 s, including TestZoneGroupBindingThroughTheRouter and TestMeZoneGroupScopedPermissions), `./internal/config` and `./internal/store/validator` (TestRoleBindingZoneGroupName), all ok.
- `drift01.log`: TestOpenAPIDrift, TestForwardedShapes, TestReferenceDrift and TestCLIReferenceDrift pass after `chore(gen)`. `lint01.log` found 2 issues in the new mount test; `lint02.log`: 0 issues after the fix. The bench guards pass.
- Review fixes (November 12, 2026). `fix-01-api-before.log`: the three new router tests against the source before the fixes fail as the review found: TestZoneGroupLabelRuleBindsToTheWrittenRead (`label removed before read 3 of the PUT: HTTP 200, zone b in group "prod"`), TestZoneGroupResolutionReadsEachZoneOnce (`record set list read DNSZones 80 times`), TestZoneGroupUnreadableZoneIsUnavailable (403 instead of 503). `fix-02-api-race.log`: `go test -race ./internal/api ./internal/rbac` after the fixes, ok (191.8 s, 1.1 s). `fix-05-cmd-before.log`: the extended mounted scenario against the source before the fixes; Kubernetes fails with `the API PUT answered zone b in group "prod"` (the administrator's API removal of a kubectl-owned label). `fix-03-cmd.log`, `fix-04-cmd-sqlite.log`: retained failures after the fixes, both 503 `current writable serving generation unavailable` on SQLite; a temporary debug print showed `serving: generation pending` behind it (a refusal before effect, also seen at sign-in), and `fix-05` shows the same 503 before the fixes, so it is not caused by them. The mounted client and the transaction test now ask again for it on repeatable requests. `fix-06-cmd.log`: one more such failure of TestServingObjectCommandResolvesZoneGroupInTransaction before its retry was widened. `fix-07-cmd.log`: `go test -race -count=3 -run ZoneGroup ./cmd/kudurru`, 9 passed, 0 skipped, 63.2 s (SQLite mount 9.6/9.9/9.6 s, Kubernetes mount 7.9/7.5/8.1 s, transaction test 3.5/3.1/3.0 s). The scenario now also proves a member's unchanged-label PUT (200), a change from prod to dev (403), a zone admin of c adding the label (403, while its PUT without the label is 200), the administrator's API removal on a kubectl-created Kubernetes zone (answer and apiserver object both without the label) and the label coming back through kubectl, and pins 401 for SQLite and 403 for Kubernetes after the binding is deleted. TestParseBindingsZoneGroupName shows that `rbac.bindings` is checked by the same rule. `fix-08-cmd-neighbors.log`: `go test -race -run 'ZoneGroup|TestServingObjectCommand|TestRunServeKubeForwardedAuthRoutes|TestServingHTTPTokenOwnerScope|ZoneAdmin|Scoped|TestRunServeKube' ./cmd/kudurru`, ok, 221.5 s. `fix-09-lint.log`: golangci-lint on `./internal/api/... ./internal/rbac/... ./cmd/kudurru/...`, 0 issues. `fix-10-drift.log`: TestOpenAPIDrift and TestBindingScopeHelp pass; no generated file changed. The bench guards pass.

Open: the dashboard has no scope picker for zone groups and does not show `zoneGroup` (later dashboard lane). Kubernetes SubjectAccessReview has no zone-group concept: SAR still names the zone, and Kubernetes RBAC on DNSZone objects (kubectl label) can change a zone's group outside kudurru's cluster-scope rule. A record set write on Kubernetes is compare-and-swapped on the record set, not on its zone, so a zone-group change that lands between the authorization and the write is not seen by that one write (the same holds for zone bindings and zone deletion; SQLite commands reauthorize in the transaction). The forced takeover on a label change also removes other managers' fields that the PUT document omits; a GitOps tool that owns the label puts it back on its next sync. The SQLite command path words a pending generation as "current writable serving generation unavailable" (503) rather than `generation_pending`, also for sign-in; the tests ask again only for requests safe to repeat. The full `./cmd/kudurru` package and `test/e2e` were not run; the integrator runs `hack/go-test.py`.
