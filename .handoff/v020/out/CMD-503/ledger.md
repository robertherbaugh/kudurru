### CMD-503 Single-shot commands answered 503 after their own or the node's commits — November 15, 2026 (D399, D407)

Two product faults and two single-shot test clients were behind the
remaining intermittent `503 unavailable` answers to single-shot commands.

A command no longer wakes the publisher after its own commit advanced the
generation (D407). Since D405 a Wake does not cancel a preparation; a Wake
noted after the preparation had read its candidate is answered by one more
preparation once it published. Nearly every HTTP command called Wake right
after its commit, when the commit's generation change had already started
the preparation that includes it, so a commit was often published twice at
the same revision and dependencies. The second publication refused, as
superseded, every command captured on the first one: the same client's
next command, marked for recapture or not. Instrumented runs (a logging
overlay of the publisher, not committed, `diag05-wake-callers.log`) show the
role binding handler's post-commit Wake followed by a second publication of
the revision. Now `servingRuntime.wakeAfterCommand` skips the Wake when
`servinghttp.CommittedPast` reports that the Store's installed revision is
past the revision of the request's current lease, in the same term; a
current command is admitted only while its lease is the current
publication, so that later revision is this commit's or a later one's, and
the generation change already schedules a preparation that reads it. An
unchanged or deduplicated result, an item of a multi-item request, a request
without a lease, and every other Wake caller (dependency watchers, member
settings pull, cluster activation and promotion, TSIG keys, blocking, edge
blocks, integration and migration steps, provider login admission,
forwarded member commands) are unchanged. The serving HTTP fixtures that
commit straight through the Store (`newServingHTTPFixture`,
`localUsersStage`, the factor and TOTP seeding helpers, the session revoke
test) no longer Wake on top of that commit either: they produced the same
double publication inside the tests.

Other wakes than a command's own still publish a source twice (the
provider coordinator's signal and the dependency watchers; lane ci measured
the same on followers, D406). So every current HTTP command, marked or not,
now checks its lease with `Publisher.CheckWriterCut` (`servinghttp.CheckWriter`):
as `CheckWriterLease`, but a lease whose publication was replaced by one of
the same cut (the same installed generation with term, revision and digest,
and the same dependency identity) is admitted while its own dependency
cursors are current and its authority is the Store's. A new generation, an
authority or writer change, a dependency change and a DNS-only publication
refuse as before, and the background acknowledgments keep the exact rule.
This is stricter than D406's forwarding rule (which does not compare the
dependency identity) because a local command reads owner values from its
lease. It is part of the replacement D399 row.

The record upsert (`POST /zones/{zone}/records`) is marked for the D399
recapture. It sends one endpoint; the command reads the zone and the record
set, merges the endpoint and authorizes the result in its own transaction,
so nothing of its first capture reaches the transaction. The API marks it in
`commandUpsertZoneRecord` with the same `api.RecapturableCommand` mark as a
whole-object write, and the root accepts the mark only for
`ZoneCommandUpsertRecord`. A record delete and a zone import keep the
immediate refusal. This was the dashboard's `e2e/stub.spec.ts:32` failure:
the record upsert right after the zone write was refused when the zone's own
status commit landed first. The replacement D399 row adds it to the marked
list.

Two tests were single-shot clients of an effect-free, documented answer.
`awaitMountedPrimarySettled` polls a zone with GET until the node's status
commits settle; its GET could be refused at capture (`503
generation_pending` with `Retry-After`, before the request was read) while
the newest of those commits was being prepared. It now asks again after
Retry-After inside its existing one-minute bound; any other answer still
fails. `TestRootMemberPasswordCommandActualHTTP` requires two audit rows for
each committed password change; `passwordChangedAt` is stored to the second
and an unchanged status writes nothing, so a second change within the same
second left one row. The fixture clock moves on a second before the second
change. No bound was widened and no unknown outcome is accepted.

Per test (rates at 329f26f4 unless noted; race binaries, each test alone,
`GOMAXPROCS=4 taskset -c 0-3`, through the heavy lock):

| Test | Command and refusal | Failures before | Decision |
| --- | --- | --- | --- |
| dashboard `stub.spec.ts:32` | `POST /zones/{name}/records`, superseded before dispatch by the zone's status commit | hosted at 329f26f4; local 1/6 | (a) product: record upsert recaptured (D399) |
| `TestServingHTTPRoleBindingsReviewedCRUD` | role binding PUT/DELETE, superseded before dispatch by the second publication of the previous commit's revision | 1/7 (bisect), 4/12 earlier | (a) product: regression of e76eb08c removed (D407) |
| `TestServingHTTPFactorsShareFailureHistoryAcrossAdapters` | factor/password command after a committed failure, same | 1/7 (bisect) | (a) D407 |
| `TestServingHTTPLocalUsersResetCASOwnershipAndDisabled` | password reset after `localUsersStage`, same (the fixture's Wake) | 1/4 | (a) D407 and the fixture |
| `TestServingHTTPPasswordChangesShareRejectedProofs/factor` | password change after the previous refused change's committed counter, same | 2/3 | (a) D407 |
| `TestRunServeQuerylogSinksStalledFailingAndReload` | `PATCH /settings` after the previous PATCH, same | 0/3 here; failed in D399-REDO full10 | (a) D407 (settings PATCH stays unmarked) |
| `TestRunServeSQLiteZonesAnswerAcrossCommits` | `POST /objects/blocklists`, `/objects/dnsrecordsets`, superseded | 0/3 race here; every hosted failure predates D399 | already fixed by D399 (whole-object POSTs are marked) |
| `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled` | GET in `awaitMountedPrimarySettled`, `generation_pending` at capture | 1/4 | (b) test follows Retry-After |
| `TestRunServeDirectAnchorsFencedAndConflictRestart` | `POST /auth/login`, superseded; every failure predates D399 | 0/3 | already fixed by D399 (login is marked) |
| `TestRunServeStaticTwinsReadOnlyPeerLossRestart` | `POST /auth/login`, superseded; the 503 failures predate D399 | 1/3, a client deadline at a runqueue wait of 35 s (load 26), not a 503 | 503 fixed by D399; the 5 s client deadline under CPU starvation stays open (2/8 at the final tree) |
| `TestServingHTTPBrowserLogoutActualTLSRoundTrip/provider-error` (hosted at 2b425e25) | `POST /auth/logout/browser` (`commitLogoutCurrent`, unmarked: it plans the provider continuation from the captured browser), after the OIDC sign-in whose `browserPublication` Wake follows its commit | 0/8 here at 329f26f4 | (a) covered by `CheckWriterCut` when the refusing publication was the same cut; browser sign-out stays unmarked; not reproduced locally |
| `TestRootMemberPasswordCommandActualHTTP` | not a 503: one audit row instead of two (same-second change) | 0/4 here; hosted at 531bffd0 | (b) test spaces the two changes |

Bisect of the serving-HTTP regression (race binaries of the commits, each
test alone, interleaved): `TestServingHTTPRoleBindingsReviewedCRUD` and
`TestServingHTTPFactorsShareFailureHistoryAcrossAdapters` together 0/16
failures at e76eb08c^, 2/16 at e76eb08c, 2/14 at 329f26f4. With the earlier
INVENTORY-REGRESS A/B (`fix-ab01-e76eb08c.log`: role bindings 4/12 at that
head, 0/12 with only e76eb08c's publisher change reverted) that names
e76eb08c, and the mechanism is the post-commit Wake above. Publisher.Run
belongs to the publication lane and is not changed; the fix is on the
command side.

Evidence under `.context/v020-final-20261110/evidence/CMD-503/`:

- `bisect01-*`: the bisect above (`bisect01-summary.log`; the run was cut by
  the tool's 30-minute limit in round 8: 8 rounds of e76eb08c^ and
  e76eb08c, 7 of the head). The failures are 503 instead of 409/429.
- `race02-head-*`: the other tests alone at 329f26f4 (`race02-head-summary.log`,
  cut by the same limit after three to four rounds). Retained failures:
  LocalUsers 1 (503 want 204), PasswordChanges 2 (503 want 401), DirectAnchors
  bootstrap 1 (GET generation_pending), StaticTwins 1 (client deadline,
  runqueue wait 35 s).
- `unit03-*`: the two new tests, 3 runs each against the base product
  (`headtest`, a go build overlay of 329f26f4's product files) and the
  change. `TestServingCommandRecaptureZoneRecordSupersededBeforeDispatch`:
  base failed 3 of 3 (503), change passed 3 of 3. `TestServingHTTPCommandCommitPublishesItsRevisionOnce`:
  base failed 3 of 3 (revisions published twice); the first build of the
  change failed 3 of 3 too, because `CommittedPast` validated the authority's installed
  generation, which carries no digest (`diag05-wake-callers.log` shows the
  role binding Wake still called); fixed before the commit.
- `race06-fix-*`, `race06-fix-summary.log`: every test above alone, 8 runs
  each, with D407 and the record recapture but before `CheckWriterCut`:
  8/8 pass for the new Wake test, role bindings, factors, root member
  password, both direct-anchor tests, querylog sinks and
  `TestRunServeSQLiteZonesAnswerAcrossCommits`; retained failures, one each:
  `TestServingHTTPPasswordChangesShareRejectedProofs/factor` (run 7, 503
  want 401), `TestServingHTTPLocalUsersResetCASOwnershipAndDisabled` (run 7,
  `POST /iam/users` 503 want 201), `TestRunServeStaticTwinsReadOnlyPeerLossRestart`
  (run 8, login transport deadline at a runqueue wait of 66 s, not a 503).
  Those two 503s led to `CheckWriterCut`.
- `race12-fix4-*`, `race11-12-summary.log`: the same tests at the final
  tree (with `CheckWriterCut`), 8 runs each: 8/8 pass for every test but
  `TestRunServeStaticTwinsReadOnlyPeerLossRestart`, 6/8: its two retained
  failures (runs 6 and 8) are the client's 5 s transport timeout on
  `POST /auth/login` after the nodes took 16 to 22 s to start with runqueue
  waits of 29 to 48 s (load 10 to 14), not a 503. The same test failed the
  same way at 329f26f4 (`race02-head-...-3.log`, a 5 s deadline on
  `POST /dns/blocking`) and in `race06` run 8.
- `race10-writer-cut-unit.log`: `TestPublisherWriterCutSurvivesASameCutPublicationOnly`
  under race, count 5: ok. The test shows `CheckWriterLease` refusing the
  same-cut publication that `CheckWriterCut` admits, and a dependency
  change, a new generation, another writer incarnation, a canceled context
  and a released lease refused. `race13-serving-lint.log`: `go test -race
  ./internal/serving ./internal/servinghttp` ok (50 s, 51 s);
  golangci-lint on both: 0 issues.
- `race11-contract-fix4.log`: the contract set below again at the final
  tree: 55 pass, 0 fail (342 s).
- `race14-*`: `TestServingHTTPBrowserLogoutActualTLSRoundTrip` alone, 8 runs
  at 329f26f4 and 8 at the final tree: all pass (the hosted failure did not
  reproduce).
- `race07-contract-fix.log`: the seven contract tests
  (`TestServingAdoptionActualHTTPOriginalWriterProofAndLease`,
  `TestServingCacheCommandRefusesOriginalAuthorityLoss`,
  `TestServingDomainCommandRetainsOriginalLeaseAndPlan`,
  `TestServingNodeCommandActualRefusesStaleAuthorityAndFinalExpiry`,
  `TestRunServeSQLiteControlSettingsRestart`,
  `TestServingSourceCommandRetainsOriginalRequest`,
  `TestServingCommandRecaptureSignInSupersededBeforeDispatch`), every
  `TestServingCommandRecapture.*` (including the new record test),
  `TestServingObjectWriteUnavailableIsEffectFree`, `TestServingItems.*`,
  `TestServingHTTPRoleBindings.*`, `TestServingHTTPLocalUsers.*`,
  `TestServingHTTPFactors.*`, `TestServingHTTPTOTP.*` and
  `TestServingHTTPSession.*` under race, CPUs 4-7: 55 tests pass, 0 fail
  (269 s).
- `norace09-*`: without the race detector, `TestRunServeSQLiteZonesAnswerAcrossCommits`
  and the D399 mounted proof `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates`:
  3/3 each pass at the D407 tree (the mounted proof's own commit-rate and
  admission assertions included).
- `e2e01-stub-head.log`: `bunx playwright test e2e/stub.spec.ts
  --repeat-each 6 --workers 2` against a `-tags=licensetest` binary of the
  base product with the built bundle: 1 failed at `stub.spec.ts:32`, 5
  passed. `e2e02-stub-fix.log`: the same with the record recapture and
  D407, `--repeat-each 12`: 12 passed.
- With `sendBeforeAdmission` in the stub and secondary specs, both specs
  `--repeat-each 10`: base binary (`e2e05-stub-secondary-head-helper.log`)
  3 of 20 failed, every one `POST /zones/.../records: HTTP 503 unavailable`
  in the secondary spec, none generation_pending; the D407 tree
  (`e2e05-...-fix2-...`) 20/20; the final tree (`e2e07-...-fix4-...`) 20/20.
  No request was asked again in any run. `e2e03-*`/`e2e04-*` (an earlier
  pair, base 12/12, D407 tree 20/20) are retained. `e2e06-*` is retained but void: that
  binary was built without the dashboard bundle and every run stopped at
  the placeholder check.
- `race04-servinghttp-api.log`: `go test -race ./internal/servinghttp
  ./internal/api`: ok (37 s, 475 s). `lint08.log`: golangci-lint v2.15.0 on
  `internal/servinghttp`, `internal/api`, `cmd/kudurru`: 0 issues. `go vet`
  and gofmt clean; `bench-guards01.log`: 14 OK.
- No route, DTO, permission or refusal classification changed, so there is
  no generated commit, and the follower contract, `permissions.yaml`,
  `internal/api/refusals.go` and `dashboard/src/lib/follower.ts` are
  unchanged.

What remains open: commands that are not marked (a settings PATCH, role
binding, user, factor and password commands, and the rest of D399's list)
are still refused when another writer's commit (zone status and lifecycle
writes, signed UPDATEs, the anchor updater, integration status) supersedes
them before dispatch; D407 removes only the refusals that the node's
duplicate publications caused. Those refusals are effect-free; the object
routes say so with Retry-After only for marked commands. A capture-time
`generation_pending` while a preparation outlasts the two-second capture
wait is unchanged (D405 limit), so other single-shot test clients may still
see it under heavy load. Many other test fixtures still Wake after their
own Store commits (`serving_http_external_users_test.go`,
`serving_member_session_settings_test.go`, `serving_member_self_test.go`
and others); they were not among the failing tests and were left alone. The
`TestRootMemberPasswordCommandActualHTTP` fix is by mechanism; the failure
did not reproduce in four local runs at the base. Protected reads (the
dashboard's `/auth/status`, `/cluster` and `/license` at first page load)
keep `CheckReadLease`, the exact publication rule, and the capture-time
refusal; D407 removes the duplicate publications that commands caused, but
a read can still be refused by the other wakes' same-source publications.
Reads were not changed here (`CheckReadLease` and D406 belong to the
publication lane). An unmarked command (`POST /iam/users` in one
instrumented e2e run) superseded by another writer's real commit is still
refused, as D399 decides. `TestRunServeStaticTwinsReadOnlyPeerLossRestart` gives its
login a 5 s client timeout, the same as the sign-in's recapture bound, so on
a starved machine a sign-in that waits for a publication can outlast the
client (2 of 8 runs here at load 10 to 14, none hosted); the test was not
changed.
